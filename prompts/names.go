// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

// The compaction prompt (spec 010), sent after the transcript it
// summarizes. Compaction is the one a new summary is asked with, which
// its context.compacted names; CompactionV1 is the one a summary that
// names none was asked with, kept so a replay builds that request again.
// Version 2 adds the heading for the questions put to a person (spec
// 039).
const (
	Compaction   Name = "compact/compact-v2"
	CompactionV1 Name = "compact/compact-v1"
)

// The advisor's texts (spec 013): its instructions when its configuration
// names none, the caller's transcript as the advisor reads it, and the
// request that carries the transcript and the caller's question.
const (
	AdvisorInstructions Name = "advisor/instructions-v1"
	// AdvisorTranscript takes Entries, each a Kind with the values it
	// shows: "text" with Agent and Text, "call" with Name and Args, and
	// "result" with Text and Cut.
	AdvisorTranscript Name = "advisor/transcript-v1"
	// AdvisorRequest takes Transcript and Question, empty for a review.
	AdvisorRequest Name = "advisor/request-v1"
)

// The tool descriptions (specs 008 and 013).
const (
	ToolRead     Name = "tools/read-v1"
	ToolWrite    Name = "tools/write-v2"
	ToolEdit     Name = "tools/edit-v2"
	ToolBash     Name = "tools/bash-v2"
	ToolGrep     Name = "tools/grep-v1"
	ToolGlob     Name = "tools/glob-v1"
	ToolWebFetch Name = "tools/web_fetch-v1"
	ToolTodo     Name = "tools/todo-v1"
	ToolSpawn    Name = "tools/spawn-v1"
	ToolMessage  Name = "tools/message-v1"
	ToolAdvisor  Name = "tools/advisor-v1"
	// ToolQuestion states the bounds of session's question constants in
	// its text, which a test holds to them (spec 039).
	ToolQuestion Name = "tools/question-v1"
	// ToolWebSearch states the bounds of the search package's constants
	// in its text, which a test holds to them (spec 040).
	ToolWebSearch Name = "tools/web_search-v1"
)

// The results of web_search (spec 040).
const (
	// WebSearchResults takes Results, each with Number, Title, URL and
	// Snippet, which may be empty.
	WebSearchResults Name = "results/web_search/results-v1"
	// WebSearchNone takes Query.
	WebSearchNone Name = "results/web_search/none-v1"
	// WebSearchRefused takes Message, the service's sentence for the
	// person, and Retry, when it may be tried again, empty when the
	// service said nothing.
	WebSearchRefused Name = "results/web_search/refused-v1"
	// WebSearchFailed takes Error.
	WebSearchFailed Name = "results/web_search/failed-v1"
	// WebSearchTimeout takes Query and Timeout.
	WebSearchTimeout Name = "results/web_search/timeout-v1"
	// WebSearchCanceled takes Query.
	WebSearchCanceled    Name = "results/web_search/canceled-v1"
	WebSearchUnavailable Name = "results/web_search/unavailable-v1"
)

// The results of the question tool (spec 039): what the model reads once
// a question call is closed, by what closed it, and the two refusals of
// a call the schema accepts.
const (
	// QuestionAnswered takes Questions, each with Number, Header,
	// Question, Chosen (the chosen labels joined), Text (the person's own
	// words) and Left, true for a question left to the agent.
	QuestionAnswered Name = "results/question/answered-v1"
	// QuestionLeft is an answer whose entries are all empty.
	QuestionLeft Name = "results/question/left-v1"
	// QuestionMessage is a person's message in place of an answer.
	QuestionMessage Name = "results/question/message-v1"
	// QuestionCanceled is an interrupt that dismissed the question.
	QuestionCanceled Name = "results/question/canceled-v1"
	// QuestionUnattended is a session nobody attends.
	QuestionUnattended Name = "results/question/unattended-v1"
	// QuestionRemoved is an answer redacted before it was read.
	QuestionRemoved Name = "results/question/removed-v1"
	// QuestionDuplicateLabel takes Number, the question's place in the
	// call from 1, and Label.
	QuestionDuplicateLabel Name = "results/question/duplicate-label-v1"
	// QuestionSecondCall takes Max, the most questions of one call.
	QuestionSecondCall Name = "results/question/second-call-v1"
)

// The texts the session fold writes into a transcript (spec 004).
const (
	TranscriptTruncated Name = "transcript/truncated-v1"
	// TranscriptSummary takes Summary.
	TranscriptSummary Name = "transcript/summary-v1"
	TranscriptCleared Name = "transcript/cleared-v1"
	// TranscriptSender and TranscriptInterrupt take the sender's Name
	// and Subject.
	TranscriptSender    Name = "transcript/sender-v1"
	TranscriptInterrupt Name = "transcript/interrupt-v1"
	// TranscriptThreadMessage takes the sending thread's FromName and
	// From.
	TranscriptThreadMessage Name = "transcript/thread-message-v1"
	// TranscriptRewound takes Turn.
	TranscriptRewound Name = "transcript/rewound-v1"
	// TranscriptAttachments takes Attachments, each with Path, MediaType
	// and Size: the files a message carries (spec 015).
	TranscriptAttachments Name = "transcript/attachments-v1"
	// TranscriptImageUnseen stands in for an image a message carries when
	// the model does not take images.
	TranscriptImageUnseen Name = "transcript/image-unseen-v1"
)

// The results the harness gives a call it does not run, or whose run
// did not finish (specs 005 and 012).
const (
	// CallDenied takes the person's Note, empty for none.
	CallDenied Name = "results/call/denied-v1"
	// CallUnknownTool takes Name.
	CallUnknownTool   Name = "results/call/unknown-tool-v1"
	CallUnknownEffect Name = "results/call/unknown-effect-v1"
	CallCanceled      Name = "results/call/canceled-v1"
	// CallFailed takes the tool's Error.
	CallFailed Name = "results/call/failed-v1"
	// CallMachineUnavailable takes the Error of a machine opened on
	// demand that could not be had (spec 009).
	CallMachineUnavailable Name = "results/call/machine-unavailable-v1"
	// CallPlanMode and CallAboveBlock are the reasons of a blocked call,
	// recorded on its agent.tool_use and read by the model as its result.
	CallPlanMode   Name = "results/call/plan-mode-v1"
	CallAboveBlock Name = "results/call/above-block-v1"
)

// The results of the thread tools (spec 013). Each takes the values its
// file names.
const (
	ThreadIdle            Name = "results/threads/idle-v1"
	ThreadStopped         Name = "results/threads/stopped-v1"
	AdvisorStopped        Name = "results/threads/advisor-stopped-v1"
	ThreadBranch          Name = "results/threads/branch-v1"
	ThreadUncommitted     Name = "results/threads/uncommitted-v1"
	ThreadCommitFailed    Name = "results/threads/commit-failed-v1"
	ThreadUnknownSubagent Name = "results/threads/unknown-subagent-v1"
	ThreadDepthExceeded   Name = "results/threads/depth-exceeded-v1"
	ThreadNoWorktrees     Name = "results/threads/no-worktrees-v1"
	ThreadTooMany         Name = "results/threads/too-many-v1"
	ThreadNotSpawned      Name = "results/threads/not-spawned-v1"
	ThreadEnded           Name = "results/threads/ended-v1"
	ThreadSubagentGone    Name = "results/threads/subagent-gone-v1"
)

// The registry's answers to a call it refuses (spec 008), and the line
// that replaces the middle of an output past its cap.
const (
	RegistryUnknownTool  Name = "results/registry/unknown-tool-v1"
	RegistryInvalidInput Name = "results/registry/invalid-input-v1"
	// RegistryInvalidJSON takes Tool, Problem and Excerpt.
	RegistryInvalidJSON Name = "results/registry/invalid-json-v1"
	// RegistryCutInput takes Tool, Limit and Excerpt.
	RegistryCutInput Name = "results/registry/cut-input-v1"
	OutputSpilled    Name = "results/spill-v1"
)

// The results the file tools share (spec 008).
const (
	FileChanged    Name = "results/files/changed-v1"
	FileUnread     Name = "results/files/unread-v1"
	FileOutside    Name = "results/files/outside-v1"
	FileDenied     Name = "results/files/denied-v1"
	FileNotFound   Name = "results/files/not-found-v1"
	FilePermission Name = "results/files/permission-v1"
	FileError      Name = "results/files/error-v1"
	FileDirectory  Name = "results/files/directory-v1"
)

// The results of read, write and edit (spec 008).
const (
	ReadDirectory     Name = "results/read/directory-v1"
	ReadBinary        Name = "results/read/binary-v1"
	ReadEmpty         Name = "results/read/empty-v1"
	ReadPastEnd       Name = "results/read/past-end-v1"
	ReadLineCut       Name = "results/read/line-cut-v1"
	ReadMoreLines     Name = "results/read/more-lines-v1"
	ReadImageTooLarge Name = "results/read/image-too-large-v1"

	WriteDone Name = "results/write/done-v1"

	EditEmptyOld  Name = "results/edit/empty-old-v1"
	EditSame      Name = "results/edit/same-v1"
	EditMissing   Name = "results/edit/missing-v1"
	EditBinary    Name = "results/edit/binary-v1"
	EditNotFound  Name = "results/edit/not-found-v1"
	EditAmbiguous Name = "results/edit/ambiguous-v1"
	EditDoneOne   Name = "results/edit/done-one-v1"
	EditDoneMany  Name = "results/edit/done-many-v1"
)

// The results of bash (spec 008).
const (
	BashEmpty      Name = "results/bash/empty-v1"
	BashDirGone    Name = "results/bash/dir-gone-v1"
	BashNotStarted Name = "results/bash/not-started-v1"
	BashBackground Name = "results/bash/background-v1"
	BashNoOutput   Name = "results/bash/no-output-v1"
	BashTimeout    Name = "results/bash/timeout-v1"
	BashCanceled   Name = "results/bash/canceled-v1"
	BashExitCode   Name = "results/bash/exit-code-v1"
	// BashMoved takes Grace, Ports, PID and Log: a foreground server
	// moved to the background (spec 045).
	BashMoved Name = "results/bash/moved-v1"
	// BashUnchecked takes Error: why a command that passed its timeout
	// was never checked for a server.
	BashUnchecked Name = "results/bash/unchecked-v1"
)

// The results of grep, glob, todo and web_fetch (spec 008).
const (
	GrepNoMatches Name = "results/grep/no-matches-v1"
	GrepMore      Name = "results/grep/more-v1"
	GlobNoMatches Name = "results/glob/no-matches-v1"
	GlobMore      Name = "results/glob/more-v1"
	SearchFailed  Name = "results/search/failed-v1"

	TodoTooMany   Name = "results/todo/too-many-v1"
	TodoNoID      Name = "results/todo/no-id-v1"
	TodoDuplicate Name = "results/todo/duplicate-v1"
	TodoNoContent Name = "results/todo/no-content-v1"
	TodoBadStatus Name = "results/todo/bad-status-v1"
	TodoEmpty     Name = "results/todo/empty-v1"
	// TodoList takes Done, Total and Items, each with Status, ID and
	// Content.
	TodoList Name = "results/todo/list-v1"

	FetchNotURL      Name = "results/web_fetch/not-url-v1"
	FetchUnavailable Name = "results/web_fetch/unavailable-v1"
	FetchCanceled    Name = "results/web_fetch/canceled-v1"
	FetchTimeout     Name = "results/web_fetch/timeout-v1"
	FetchFailed      Name = "results/web_fetch/failed-v1"
	FetchNotText     Name = "results/web_fetch/not-text-v1"
	// FetchPage takes URL, Status, ContentType, Body (ending in a
	// newline), Truncated and Max, the body limit in MiB.
	FetchPage Name = "results/web_fetch/page-v1"
)

// The system parts of a request (spec 011): the context block a machine
// records when it attaches, the block that names a session's
// repositories before its first machine does, an instruction file, the
// skills index and a memory store, and the lines that mark instructions
// cut at their limits.
const (
	// ContextBlock takes Workdir, OS, Arch, Cella, Environment, Date,
	// Git, Branch, Head, Modified, Untracked and Commits.
	ContextBlock Name = "context/context-v1"
	// ContextRepositories takes Repositories, each with URL, Ref, Branch
	// and Dir, the directory under the working directory, empty for the
	// working directory itself.
	ContextRepositories Name = "context/repositories-v1"
	// ContextInstructions takes Path and Body.
	ContextInstructions  Name = "context/instructions-v1"
	InstructionCut       Name = "context/instruction-cut-v1"
	InstructionsTotalCut Name = "context/instructions-total-cut-v1"
	// ContextSkills takes Skills, each with Name, Description and Path.
	ContextSkills Name = "context/skills-v1"
	// ContextMemory takes Name, ReadOnly, Path and Description.
	ContextMemory Name = "context/memory-v1"
)
