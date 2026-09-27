Runs a shell command on the machine with `/bin/sh -c` and returns its standard output and standard error, combined in the order written, followed by its exit code.

Use it to build, test, run programs, use git, and inspect the machine. Prefer `grep` and `glob` to search file contents and names, `read` to look at a file, and `edit` and `write` to change one: they need no binary on the machine and their results are made for you.

Inputs:
- `command`: the command.
- `timeout_ms`: milliseconds before the command and its process group are killed. Default 120000, at most 600000.
- `background`: start the command detached and return at once. Default false.
- `description`: what the command does, in a few words.

Behavior:
- The working directory persists between calls: a command starts in the directory the previous command of this thread ended in, so `cd` carries over. Environment variables do not persist; set them in the command that needs them.
- A non-zero exit code is reported in the result and is not an error of the tool.
- A command that passes its timeout is killed with every process it started.
- The command reads no input. Do not run commands that wait for a person to type, and pass flags that make tools non-interactive.
- Output past the tool's limit is written to a spill file; the result shows its start and end and names the file, which `read` opens.

Background jobs:
- Use `background` for anything that keeps running: a server, a watcher, a long build you check on later. A foreground server never returns and is killed at the timeout.
- A background call returns the job's pid and the path of its log, which receives the job's output. Read the log with `read`, and stop the job with `kill -TERM -<pid>`, which signals its whole process group.
- Background jobs are stopped when the session ends.
