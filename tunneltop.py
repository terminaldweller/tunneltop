#!/usr/bin/env python
"""A top-like program for monitoring ssh tunnels or any tunnels"""
# TODO- the disabled coloring is not working
# TODO- quit doesnt work
# TODO- we are reviving dead tunnels
import argparse
import asyncio
import copy
import curses
import enum
import os
import signal
import sys
import tomllib
import typing


class Argparser:  # pylint: disable=too-few-public-methods
    """Argparser class."""

    def __init__(self):
        self.parser = argparse.ArgumentParser()
        self.parser.add_argument(
            "--config",
            "-c",
            type=str,
            help="The path to the .tunneltop.toml file,"
            " defaults to $HOME/.tunneltop.toml",
            default="~/.tunneltop.toml",
        )
        self.parser.add_argument(
            "--noheader",
            "-n",
            type=bool,
            help="Dont print the header in the output",
            default=False,
        )
        self.parser.add_argument(
            "--delay",
            "-d",
            type=float,
            help="The delay between redraws in seconds, defaults to 5 seconds",
            default=5,
        )
        self.args = self.parser.parse_args()


# pylint: disable=too-few-public-methods
class Colors(enum.EnumType):
    """static color definitions"""

    purple = "\033[95m"
    blue = "\033[94m"
    green = "\033[92m"
    yellow = "\033[93m"
    red = "\033[91m"
    grey = "\033[1;37m"
    darkgrey = "\033[1;30m"
    cyan = "\033[1;36m"
    ENDC = "\033[0m"
    BOLD = "\033[1m"
    UNDERLINE = "\033[4m"
    blueblue = "\x1b[38;5;24m"
    greenie = "\x1b[38;5;23m"
    goo = "\x1b[38;5;22m"
    screen_clear = "\033c\033[3J"
    hide_cursor = "\033[?25l"


# pylint: disable=too-many-locals
def ffs(
    offset: int,
    header_list: typing.Optional[typing.List[str]],
    numbered: bool,
    no_color: bool,
    *args,
) -> typing.List[str]:
    """A simple columnar printer"""
    max_column_width = []
    lines = []
    numbers_f: typing.List[int] = []
    dummy = []

    if no_color or not sys.stdout.isatty():
        greenie = ""
        bold = ""
        endc = ""
        goo = ""
        blueblue = ""
    else:
        greenie = Colors.greenie
        bold = Colors.BOLD
        endc = Colors.ENDC
        goo = Colors.goo
        blueblue = Colors.blueblue

    for arg in args:
        max_column_width.append(max(len(repr(argette)) for argette in arg))

    if header_list is not None:
        if numbered:
            numbers_f.extend(range(1, len(args[-1]) + 1))
            max_column_width.append(
                max(len(repr(number)) for number in numbers_f)
            )
            header_list.insert(0, "idx")

        index = range(0, len(header_list))
        for header, width, i in zip(header_list, max_column_width, index):
            max_column_width[i] = max(len(header), width) + offset

        for i in index:
            dummy.append(
                greenie
                + bold
                + header_list[i].ljust(max_column_width[i])
                + endc
            )
        lines.append("".join(dummy))
        dummy.clear()

    index2 = range(0, len(args[-1]))
    for i in index2:
        if numbered:
            dummy.append(
                goo + bold + repr(i).ljust(max_column_width[0]) + endc
            )
            for arg, width in zip(args, max_column_width[1:]):
                dummy.append(blueblue + (arg[i]).ljust(width) + endc)
        else:
            for arg, width in zip(args, max_column_width):
                dummy.append(blueblue + (arg[i]).ljust(width) + endc)
        lines.append("".join(dummy))
        dummy.clear()
    return lines


def render(
    data_cols: typing.Dict[str, typing.Dict[str, str]],
    tasks: typing.List[asyncio.Task],
    stdscr,
    sel: int,
):
    """Render the text"""
    lines = ffs(
        2,
        ["NAME", "ADDRESS", "PORT", "STATUS", "STDOUT", "STDERR"],
        False,
        True,
        [v["name"] for _, v in data_cols.items()],
        [v["address"] for _, v in data_cols.items()],
        [repr(v["port"]) for _, v in data_cols.items()],
        [v["status"] for _, v in data_cols.items()],
        [v["stdout"] for _, v in data_cols.items()],
        [v["stderr"] for _, v in data_cols.items()],
    )
    iterator = iter(lines)
    stdscr.addstr(1, 1, lines[0], curses.color_pair(3))
    next(iterator)
    for i, line in enumerate(iterator):
        try:
            line_content = stdscr.instr(sel + 2, 1).decode("utf-8")
            name: str = line_content[: line_content.find(" ")]
        finally:
            name = ""
        if i == sel:
            stdscr.addstr(
                (2 + i) % (len(lines) + 1),
                1,
                line,
                curses.color_pair(2)
                if name not in tasks
                else curses.color_pair(5),
            )
        else:
            stdscr.addstr(
                2 + i,
                1,
                line,
                curses.color_pair(1)
                if name not in tasks
                else curses.color_pair(4),
            )
        stdscr.addstr("\n")
    stdscr.box()


def curses_init():
    """Initialize ncurses"""
    stdscr = curses.initscr()
    curses.start_color()
    curses.use_default_colors()
    curses.curs_set(False)
    curses.noecho()
    curses.cbreak()
    stdscr.keypad(True)
    curses.halfdelay(20)
    curses.init_pair(1, curses.COLOR_GREEN, curses.COLOR_BLACK)
    curses.init_pair(2, curses.COLOR_BLACK, curses.COLOR_GREEN)
    curses.init_pair(3, curses.COLOR_BLUE, curses.COLOR_BLACK)
    curses.init_pair(4, curses.COLOR_CYAN, curses.COLOR_BLACK)
    curses.init_pair(5, curses.COLOR_BLACK, curses.COLOR_CYAN)

    return stdscr


class TunnelManager:
    """The tunnel top class"""

    def __init__(self):
        self.argparser = Argparser()
        self.data_cols: typing.Dict[
            str, typing.Dict[str, str]
        ] = self.read_conf()
        self.tunnel_tasks: typing.List[asyncio.Task] = []
        self.tunnel_test_tasks: typing.List[asyncio.Task] = []
        self.scheduler_task: asyncio.Task
        self.scheduler_table: typing.Dict[
            str, int
        ] = self.init_scheduler_table()
        # we use this when its time to quit. this will prevent any
        # new tasks from being scheduled
        self.are_we_dying: bool = False

    def init_scheduler_table(self) -> typing.Dict[str, int]:
        """initialize the scheduler table"""
        result: typing.Dict[str, int] = {}
        for key, value in self.data_cols.items():
            if "test_interval" in value and value["test_command"] != "":
                result[key] = 0

        return result

    async def stop_task(
        self,
        delete_task: asyncio.Task,
        task_list: typing.List[asyncio.Task],
        delete: bool = True,
    ):
        """Remove the reference"""
        delete_index: int = -1
        delete_task.cancel()
        self.write_log(f"{delete_task.get_name()} is being cancelled\n")
        await asyncio.sleep(0)
        for i, task in enumerate(task_list):
            if task.get_name() == delete_task.get_name():
                delete_index = i
                break

        if delete and delete_index >= 0:
            task_list.remove(self.tunnel_tasks[delete_index])

    def read_conf(self) -> typing.Dict[str, typing.Dict[str, str]]:
        """Read the config file"""
        data_cols: typing.Dict[str, typing.Dict[str, str]] = {}
        with open(
            os.path.expanduser(self.argparser.args.config), "rb"
        ) as conf_file:
            data = tomllib.load(conf_file)
            for key, value in data.items():
                data_cols[key] = {
                    "name": key,
                    "address": value["address"],
                    "port": value["port"],
                    "command": value["command"],
                    "status": "UNKN",
                    "test_command": value["test_command"],
                    "test_command_result": value["test_command_result"],
                    "test_interval": value["test_interval"],
                    "test_timeout": value["test_timeout"],
                    "stdout": "",
                    "stderr": "",
                }
        return data_cols

    async def run_subprocess(self, cmd: str) -> typing.Tuple[bytes, bytes]:
        """Run a command"""
        try:
            proc = await asyncio.create_subprocess_exec(
                *cmd.split(" "),
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
            )

            return await proc.communicate()
        except asyncio.TimeoutError:
            proc.terminate()
            raise
        except asyncio.CancelledError:
            proc.terminate()
            raise

    async def run_test_coro(
        self, cmd: str, task_name: str
    ) -> typing.Tuple[bytes, bytes]:
        """Run a test command"""
        try:
            stdout, stderr = await self.run_subprocess(cmd)
            stdout_str: str = stdout.decode("utf-8").strip("\n").strip('"')
            stderr_str: str = stderr.decode("utf-8").strip("\n").strip('"')

            self.data_cols[task_name]["stdout"] = stdout_str
            self.data_cols[task_name]["stderr"] = stderr_str
            if stdout_str == self.data_cols[task_name]["test_command_result"]:
                self.data_cols[task_name]["status"] = "UP"
            else:
                self.data_cols[task_name]["status"] = "DOWN"

            return stdout, stderr
        except asyncio.TimeoutError:
            self.data_cols[task_name]["status"] = "TMOUT"
            self.data_cols[task_name]["stdout"] = "-"
            self.data_cols[task_name]["stderr"] = "-"
            raise

    async def tunnel_procs(
        self,
    ) -> typing.List[asyncio.Task]:
        """run all the tunnels in the background as separate subprocesses"""
        tasks: typing.List[asyncio.Task] = []
        for _, value in self.data_cols.items():
            tasks.append(
                asyncio.create_task(
                    self.run_subprocess(value["command"]), name=value["name"]
                ),
            )
            await asyncio.sleep(0)

        return tasks

    async def sighup_handler_async_worker(self, data_cols_new) -> None:
        """Handles the actual updating of tasks when we get SIGTERM"""
        delete_task: typing.Optional[asyncio.Task] = None
        for k, value in data_cols_new.items():
            if k not in self.data_cols:
                self.tunnel_tasks.append(
                    asyncio.create_task(
                        self.run_subprocess(value["command"]), name=k
                    )
                )
                await asyncio.sleep(0)
                self.data_cols[k] = copy.deepcopy(value)
                if k in self.scheduler_table:
                    self.scheduler_table[k] = 0
            else:
                if (
                    self.data_cols[k]["command"] != data_cols_new[k]["command"]
                    or self.data_cols[k]["port"] != data_cols_new[k]["port"]
                    or self.data_cols[k]["address"]
                    != data_cols_new[k]["address"]
                ):
                    for task in self.tunnel_tasks:
                        if task.get_name() == k:
                            delete_task = task
                            break
                            # task.cancel()
                            # await asyncio.sleep(0)

                    if delete_task is not None:
                        await self.stop_task(delete_task, self.tunnel_tasks)
                        delete_task = None
                    self.data_cols[k] = copy.deepcopy(data_cols_new[k])
                    self.tunnel_tasks.append(
                        asyncio.create_task(
                            self.run_subprocess(value["command"]), name=k
                        )
                    )
                    if k in self.scheduler_table:
                        self.scheduler_table[k] = 0
                    await asyncio.sleep(0)

        for k, _ in self.data_cols.items():
            if k not in data_cols_new:
                for task in self.tunnel_tasks:
                    if task.get_name() == k:
                        # task.cancel()
                        # await asyncio.sleep(0)
                        delete_task = task
                        break
                if delete_task is not None:
                    await self.stop_task(delete_task, self.tunnel_tasks)
                    delete_task = None
                del self.data_cols[k]
                if k in self.scheduler_table:
                    del self.scheduler_table[k]

    async def sighup_handler(self) -> None:
        """SIGHUP handler. we want to reload the config."""
        # type: ignore # pylint: disable=E0203
        data_cols_new: typing.Dict[str, typing.Dict[str, str]] = {}
        data_cols_new = self.read_conf()
        await self.sighup_handler_async_worker(data_cols_new)

    def write_log(self, log: str):
        """A simple logger"""
        with open(
            "/home/devi/devi/abbatoir/hole15/log",
            "a",
            encoding="utf-8",
        ) as logfile:
            logfile.write(log)

    async def restart_task(self, line_content: str) -> None:
        """restart a task"""
        name: str = line_content[: line_content.find(" ")]
        # was_cancelled: bool = False
        for task in self.tunnel_tasks:
            if task.get_name() == name:
                # was_cancelled = task.cancel()
                # self.write_log(f"was_cancelled: {was_cancelled}")
                await self.stop_task(task, self.tunnel_tasks)
                # await task
                # await asyncio.sleep(0)
                for _, value in self.data_cols.items():
                    if value["name"] == name and task.cancelled():
                        self.tunnel_tasks.append(
                            asyncio.create_task(
                                self.run_subprocess(value["command"]),
                                name=value["name"],
                            )
                        )
                        await asyncio.sleep(0)

    async def flip_task(self, line_content: str) -> None:
        """flip a task"""
        name: str = line_content[: line_content.find(" ")]
        # was_cancelled: bool = False
        was_active: bool = False
        for task in self.tunnel_tasks:
            if task.get_name() == name:
                await self.stop_task(task, self.tunnel_tasks)
                # was_cancelled = task.cancel()
                # await asyncio.sleep(0)
                # self.write_log(f"was_cancelled: {was_cancelled}")
                # await task
                was_active = True
                break

        if not was_active:
            for _, value in self.data_cols.items():
                if value["name"] == name:
                    self.tunnel_tasks.append(
                        asyncio.create_task(
                            self.run_subprocess(value["command"]),
                            name=value["name"],
                        )
                    )
                    await asyncio.sleep(0)

    async def quit(self) -> None:
        """Cleanly quit the applicaiton"""
        # scheduler checks for this to stop running new tests
        # when we want to quit
        self.are_we_dying = True

        for task in asyncio.all_tasks():
            task.cancel()
            await asyncio.sleep(0)
        try:
            await asyncio.gather(*asyncio.all_tasks())
        finally:
            sys.exit(0)

    async def scheduler(self) -> None:
        """scheduler manages running the tests and reviving dead tunnels"""
        try:
            while True:
                if self.are_we_dying:
                    return
                for key, value in self.scheduler_table.items():
                    if value == 0 and key not in self.tunnel_test_tasks:
                        tunnel_entry = self.data_cols[key]
                        test_task = asyncio.create_task(
                            asyncio.wait_for(
                                self.run_test_coro(
                                    tunnel_entry["test_command"],
                                    tunnel_entry["name"],
                                ),
                                timeout=float(tunnel_entry["test_timeout"]),
                            ),
                            name=key,
                        )
                        self.tunnel_test_tasks.append(test_task)
                        self.scheduler_table[key] = int(
                            tunnel_entry["test_interval"]
                        )
                        await asyncio.sleep(0)
                    else:
                        self.scheduler_table[key] = (
                            self.scheduler_table[key] - 1
                        )

                # we are using a 1 second ticker. basically the scheduler
                # runs every second instead of as fast as it can
                await asyncio.sleep(1)
        except asyncio.CancelledError:
            pass

    async def tui_loop(self) -> None:
        """the tui loop"""
        sel: int = 0
        try:
            stdscr = curses_init()
            # we spawn the tunnels and the test scheduler put them
            # in the background and then run the TUI loop
            self.tunnel_tasks = await self.tunnel_procs()
            self.scheduler_task = asyncio.create_task(
                self.scheduler(), name="scheduler"
            )
            loop = asyncio.get_event_loop()
            loop.add_signal_handler(
                signal.SIGHUP,
                lambda: asyncio.create_task(self.sighup_handler()),
            )

            while True:
                stdscr.clear()
                render(self.data_cols, self.tunnel_tasks, stdscr, sel)
                char = stdscr.getch()

                if char == ord("j") or char == curses.KEY_DOWN:
                    sel = (sel + 1) % len(self.data_cols)
                elif char == ord("k") or char == curses.KEY_UP:
                    sel = (sel - 1) % len(self.data_cols)
                elif char == ord("g") or char == curses.KEY_UP:
                    sel = 0
                elif char == ord("G") or char == curses.KEY_UP:
                    sel = len(self.data_cols) - 1
                elif char == ord("r"):
                    line_content = stdscr.instr(sel + 2, 1)
                    await self.restart_task(line_content.decode("utf-8"))
                elif char == ord("q"):
                    await self.quit()
                elif char == ord("s"):
                    line_content = stdscr.instr(sel + 2, 1)
                    await self.flip_task(line_content.decode("utf-8"))

                stdscr.refresh()
                await asyncio.sleep(0)
        finally:
            curses.nocbreak()
            stdscr.keypad(False)
            curses.echo()
            curses.endwin()
            # tasks = asyncio.all_tasks()
            # for task in tasks:
            #     task.cancel()
            await self.quit()


def main() -> None:
    """entry point"""
    tunnel_manager = TunnelManager()
    asyncio.run(tunnel_manager.tui_loop())


if __name__ == "__main__":
    main()
