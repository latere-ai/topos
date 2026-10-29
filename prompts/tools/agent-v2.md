Starts one of your subagents on a task and returns its final answer once it is done. The subagent works on this same machine and reads the files you wrote, but it does not see this conversation, so give it a complete task: what to do, what it needs to know that you know, and what its answer should hold.

Its answer comes back to you, not to the person. Read it, check what matters, and say in your own reply what the person needs from it.

Use it for a part of the work a focused subagent does well on its own, such as a search through many files, or a part that needs nothing of this conversation. Do the work yourself when it takes a few steps.

Several agent calls in one step run at the same time: start the subagents of independent parts of the work in one step, and make a call that needs another subagent's answer in a later step.

A subagent keeps what it did. The result names its id: send it more work with `send_message` rather than starting another subagent on the same work.

Inputs:
- `subagent`: which of your subagents runs the task.
- `task`: the subagent's first message, complete on its own.
- `isolation`: optional. `shared`, the default, works in your working directory. `worktree` works on a branch of its own, from your last commit, and the result names the branch to merge.
- `tools`: optional, the names of the tools the subagent may use, of those it holds.
