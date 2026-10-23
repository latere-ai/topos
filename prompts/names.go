// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

// The compaction prompt (spec 010), sent after the transcript it
// summarizes. Compaction is the one a new summary is asked with, which
// its context.compacted names; CompactionV1 is the one a summary that
// names none was asked with, kept so a replay builds that request again.
// Version 2 adds the heading for the questions put to a person (spec
// 039). CompactionHistory, version 3, is the one a thread that holds the
// history tool asks with (spec 084): the messages a summary replaces stay
// readable, so a long exact text may be named rather than copied.
const (
	Compaction        Name = "compact/compact-v2"
	CompactionV1      Name = "compact/compact-v1"
	CompactionHistory Name = "compact/compact-v3"
)

// Title is the request that asks a model for a session's title after a
// turn (spec 083), which its model.request names as its prompt_version.
// It takes Title, the title the harness last gave the session, "" on the
// first naming, which offers no KEEP; Messages, the person's latest
// messages, each a Text, the names of its Files and the agent's Answer;
// and Max, the most characters a title holds.
const Title Name = "title/title-v1"

// The texts the harness adds after a request's fold for that request
// alone, which no event of the log holds (spec 062). A model.request that
// carried one names it and its data in its reminder, so a replay renders
// it again.
const (
	// ReminderToolAsText takes Tool, the offered tool the last response
	// wrote a call of into its text.
	ReminderToolAsText Name = "reminders/tool-as-text-v1"
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
	// ToolRead version 2 names the office documents read reads as text
	// (spec 079). ToolReadParsed, version 3, is read's description where a
	// parsing service reads a PDF as text (spec 082): it adds pages and
	// the PDF.
	ToolRead       Name = "tools/read-v2"
	ToolReadParsed Name = "tools/read-v3"
	ToolWrite      Name = "tools/write-v2"
	ToolEdit       Name = "tools/edit-v2"
	ToolBash       Name = "tools/bash-v2"
	ToolGrep       Name = "tools/grep-v1"
	ToolGlob       Name = "tools/glob-v1"
	ToolWebFetch   Name = "tools/web_fetch-v1"
	ToolTodo       Name = "tools/todo-v1"
	ToolSpawn      Name = "tools/spawn-v1"
	ToolMessage    Name = "tools/message-v1"
	ToolAdvisor    Name = "tools/advisor-v1"
	// ToolQuestion states the bounds of session's question constants in
	// its text, which a test holds to them (spec 039).
	ToolQuestion Name = "tools/question-v1"
	// ToolWebSearch states the bounds of the search package's constants
	// in its text, which a test holds to them (spec 047).
	ToolWebSearch Name = "tools/web_search-v1"
	// ToolPublish states the publish tool's wait in its text, which a
	// test holds to its constant (spec 043), its app input, and a
	// release that publishes and releases in one call (spec 059). It
	// promises no public address: who may open an app is the app's own
	// setting, which a result names and a call with access changes once
	// the person approves it (spec 043). It tells the model to name the
	// site and to give it an address the first time it publishes one, and
	// states the bound of an address, which a test holds to its constant
	// (spec 067). It tells the model that one call publishes or releases
	// and then changes who can open the app, and to set access on the call
	// that publishes a site the person already asked to share or keep
	// private (spec 043).
	ToolPublish Name = "tools/publish-v6"
	// ToolRequestConnection states the bound of a reason, which a test
	// holds to its constant (spec 069).
	ToolRequestConnection Name = "tools/request_connection-v1"
	// ToolHistory states the bounds of a query's answer, which a test
	// holds to the harness's constants (spec 084).
	ToolHistory Name = "tools/history-v1"
	// ToolTrigger states the bounds of a proposal's name and why and of a
	// list, which a test holds to their constants (spec 086).
	ToolTrigger Name = "tools/trigger-v3"
	// ToolWake states the bounds of a wake's time, title and note and of
	// a chain of wakes, which a test holds to their constants (spec 086).
	ToolWake Name = "tools/wake-v1"
	// ToolNotify states the bound of a notice's title, which a test holds
	// to its constant (spec 086).
	ToolNotify Name = "tools/notify-v1"
)

// The results of the notify tool (spec 086).
const (
	// NotifyNoticed takes Title, the line the person reads.
	NotifyNoticed Name = "results/notify/noticed-v1"
	// NotifyAgain answers a second notify of a turn.
	NotifyAgain Name = "results/notify/again-v1"
	// NotifyInvalid takes Detail, what the call's input got wrong.
	NotifyInvalid Name = "results/notify/invalid-v1"
)

// The results of the wake tool (spec 086).
const (
	// WakeInvalid takes Detail, what the call's input got wrong.
	WakeInvalid Name = "results/wake/invalid-v1"
	// WakeTooSoon takes Code, Min, the floor in words, and Earliest.
	WakeTooSoon Name = "results/wake/too-soon-v1"
	// WakeTooLate takes Code, Max, the ceiling in words, and Latest.
	WakeTooLate Name = "results/wake/too-late-v1"
	// WakeIsSet takes At, Count, the wake's place in the chain, Max,
	// Earliest and Latest, and Held, whether the person has paused every
	// trigger.
	WakeIsSet Name = "results/wake/set-v1"
	// WakeCanceled and WakeNone are a cancel's, of a wake and of none.
	WakeCanceled Name = "results/wake/canceled-v1"
	WakeNone     Name = "results/wake/none-v1"
	// WakeNotSet takes Code and Message, why the server refused a set or a
	// cancel.
	WakeNotSet Name = "results/wake/not-set-v1"
	// WakeMany is the reason a set past the chain's bound waits for the
	// person: Count, its place, and Max.
	WakeMany Name = "results/wake/many-v1"
)

// The results of the trigger tool (spec 086).
const (
	// TriggerInvalid takes Detail, what the call's input got wrong.
	TriggerInvalid Name = "results/trigger/invalid-v1"
	// TriggerRefused takes Code and Message, why the installation would
	// not let the person hold the routine, asked before anyone was.
	TriggerRefused Name = "results/trigger/refused-v1"
	// TriggerMade takes Title and Next, when the routine runs next, empty
	// when it names no time.
	TriggerMade Name = "results/trigger/made-v1"
	// TriggerNotMade takes Code and Message, why a routine the person
	// allowed was refused all the same.
	TriggerNotMade Name = "results/trigger/not-made-v1"
	// TriggerList takes Triggers, each an ID, Title, Name, Schedule,
	// TimeZone, Product, Ask, At, Target, TargetTitle, Paused, Next and
	// Offered.
	TriggerList Name = "results/trigger/list-v2"
	// TriggerRefusedChange takes Action, Code and Message, why a change,
	// resume or delete the person would be asked about cannot be done.
	TriggerRefusedChange Name = "results/trigger/refused-change-v1"
	// TriggerNotDone takes Action, Held, whether the person allowed it,
	// Code and Message, why a change, pause, resume or delete was refused
	// when it ran.
	TriggerNotDone Name = "results/trigger/not-done-v1"
	// TriggerChanged and TriggerResumed take Title and Next, when the
	// trigger runs next, empty when it names no time.
	TriggerChanged Name = "results/trigger/changed-v1"
	TriggerResumed Name = "results/trigger/resumed-v1"
	// TriggerPaused and TriggerDeleted take Title.
	TriggerPaused  Name = "results/trigger/paused-v1"
	TriggerDeleted Name = "results/trigger/deleted-v1"
)

// The results of request_connection (spec 069).
const (
	// RequestConnectionAsked takes the Title of the connection the person
	// is asked to connect.
	RequestConnectionAsked Name = "results/request_connection/asked-v1"
	// RequestConnectionNotAvailable takes the Name a call named and Names,
	// the names of the connections the session may ask for, joined.
	RequestConnectionNotAvailable Name = "results/request_connection/not-available-v1"
)

// The results of the publish tool (spec 043). Path is the folder as the
// call named it, Wait a duration, Tag a release's tag, and Code and
// Message the app host's code and sentence.
const (
	// PublishReady takes Path, Preview, URL and Visibility, who may open
	// the app as the host names it, empty when it names none.
	PublishReady Name = "results/publish/ready-v2"
	// PublishBuilding takes Path, Wait and Preview, empty when the host
	// started no build.
	PublishBuilding Name = "results/publish/building-v1"
	// PublishFailed takes Path, Code, Message and Log, the end of the
	// build log, empty when there is none.
	PublishFailed Name = "results/publish/failed-v1"
	// PublishReleased takes Tag, URL and Visibility.
	PublishReleased Name = "results/publish/released-v2"
	// PublishPending takes Tag and Wait.
	PublishPending Name = "results/publish/pending-v1"
	// PublishRefused takes Tag and Reason.
	PublishRefused Name = "results/publish/refused-v1"
	// PublishReleaseFailed takes Tag, Code and Message.
	PublishReleaseFailed Name = "results/publish/release-failed-v1"
	PublishNoPreview     Name = "results/publish/no-preview-v1"
	// PublishNotFolder takes Path.
	PublishNotFolder Name = "results/publish/not-folder-v1"
	// PublishUnavailable takes Reason.
	PublishUnavailable Name = "results/publish/unavailable-v1"
	// PublishError takes Path and Error.
	PublishError Name = "results/publish/error-v1"
	// PublishNotAttached takes App, the slug a call named, and Apps, the
	// slugs the session may publish to, joined (spec 059).
	PublishNotAttached Name = "results/publish/not-attached-v1"
	// PublishAppRequired takes Apps, the slugs a release that names no
	// app could mean, joined.
	PublishAppRequired Name = "results/publish/app-required-v2"
	// PublishNotInCheckout takes Path, Dir, the checkout's directory under
	// the working directory, and App.
	PublishNotInCheckout Name = "results/publish/not-in-checkout-v1"
	// PublishBehindLive takes App, Live, the live commit abbreviated,
	// Attached, Dir, an attached app's checkout under the working
	// directory, and URL, the repository the app's git directory fetches
	// from.
	PublishBehindLive Name = "results/publish/behind-live-v2"
	// PublishReleaseBuildFailed takes Path, Code, Message and Log, as
	// PublishFailed: the build a release waited for failed, and nothing
	// was released.
	PublishReleaseBuildFailed Name = "results/publish/release-build-failed-v1"
	// PublishReleaseNotStarted takes Path and Wait: the host started no
	// build of what a release pushed, and nothing was released.
	PublishReleaseNotStarted Name = "results/publish/release-not-started-v1"
	// PublishAccessChanged takes App, URL and Visibility, who can open the
	// app as the host names it after a call with access (spec 043).
	PublishAccessChanged Name = "results/publish/access-changed-v1"
	// PublishAccessRefused takes App; Role, true when the host refused
	// because the app is an organization's and the caller's role there
	// does not reach the change; Error, why else nothing was changed; and
	// Reason, the token the host's refusal names, empty when it names
	// none.
	PublishAccessRefused Name = "results/publish/access-refused-v1"
	// PublishAccessApp takes Access and Apps, the slugs a call with access
	// that names no app could mean, joined, empty when the thread has
	// published none.
	PublishAccessApp Name = "results/publish/access-app-v1"
	// PublishAccessNotChanged follows the text of a publish or a release
	// that succeeded when the change of who can open the app the same call
	// asked for failed. It takes App, Role, Error and Reason, as
	// PublishAccessRefused, and Visibility, who can open the app as far as
	// the call read it, empty when the host names none.
	PublishAccessNotChanged Name = "results/publish/access-not-changed-v1"
)

// The results of web_search (spec 047).
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

// The texts of the history tool (spec 084). HistoryHead takes Kind and
// the values each kind names: message, answer, interrupt and title a
// sender's Name and Subject; call, result and answer an ID; call and
// result a Tool; result an Outcome; task and removed none. Every render
// passes all six.
// HistoryEntry takes Seq, Time, Head and Body, empty for an entry with
// none, and HistoryMatch Seq, Time, Head and Excerpt, the text around a
// query's first match, with Before and After, whether the entry's text
// goes on beyond it.
const (
	HistoryHead  Name = "results/history/head-v1"
	HistoryEntry Name = "results/history/entry-v1"
	HistoryMatch Name = "results/history/match-v1"
	// HistoryFiles takes Paths, the paths of a message's files, joined.
	HistoryFiles Name = "results/history/files-v1"
	// HistoryPicture takes Path, the file a result's picture was read
	// from, empty when the result names none.
	HistoryPicture Name = "results/history/picture-v1"
	// HistoryAnswers takes Answers, each with Number, Chosen (the chosen
	// labels joined), Text (the person's own words) and Left, true for a
	// question left to the agent.
	HistoryAnswers Name = "results/history/answers-v1"
	// HistoryCut takes Total, the bytes of an entry, and Kept, the bytes
	// an answer shows of it.
	HistoryCut Name = "results/history/cut-v1"
	// HistoryMore takes Input, the read that goes on where an answer
	// stopped, as JSON.
	HistoryMore Name = "results/history/more-v1"
	// HistoryNone takes From and To, the range read, To 0 for a range
	// read to its end, and Last, the last event before the call.
	HistoryNone Name = "results/history/none-v1"
	// HistoryNoCall takes ID.
	HistoryNoCall Name = "results/history/no-call-v1"
	// HistoryNoMatch takes From, To and Query.
	HistoryNoMatch Name = "results/history/no-match-v1"
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
	// TranscriptSummaryHistory takes Summary, and From and To, the events
	// it covers, and TranscriptClearedHistory takes ID, the cleared call's:
	// the texts of a thread that holds the history tool, which name what
	// it reads back (spec 084).
	TranscriptSummaryHistory Name = "transcript/summary-v2"
	TranscriptClearedHistory Name = "transcript/cleared-v2"
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
	// WakeMessage leads the message of a session's own wake (spec 086):
	// Set and Due, when the wake was set and when it was due, Count, its
	// place in the chain of wakes with no word from the person, and Late
	// with Fired, whether and when it came late.
	WakeMessage Name = "wake/message-v1"
	// TranscriptAttachmentsDevice takes Attachments as
	// TranscriptAttachments does: the files a message carries on a
	// person's own computer, kept outside the working directory (spec
	// 080).
	TranscriptAttachmentsDevice Name = "transcript/attachments-device-v1"
	// TranscriptAttachmentText takes Path, Fence and Text: the text of a
	// short text file a message carries, after the line that names it
	// (spec 079), set between fences no line of it closes.
	TranscriptAttachmentText Name = "transcript/attachment-text-v1"
	// TranscriptImageUnseen stands in for an image a message carries when
	// the model does not take images.
	TranscriptImageUnseen Name = "transcript/image-unseen-v1"
	// TranscriptApprovalRequested takes Host, Port, More and Ask: a
	// command's connection the egress gateway refused (spec 052).
	TranscriptApprovalRequested Name = "transcript/approval-requested-v1"
	// TranscriptApprovalAllowed, TranscriptApprovalDenied and
	// TranscriptApprovalUnavailable take Host and Note: the person's answer
	// to a refused connection, and an allow the network could not take.
	TranscriptApprovalAllowed     Name = "transcript/approval-allowed-v1"
	TranscriptApprovalDenied      Name = "transcript/approval-denied-v1"
	TranscriptApprovalUnavailable Name = "transcript/approval-unavailable-v1"
	// TranscriptMachineRestarted takes OutOfMemory, whether the machine is
	// known to have stopped for memory: the session's machine stopped and
	// runs again with its files (spec 009).
	TranscriptMachineRestarted Name = "transcript/machine-restarted-v1"
	// TranscriptMachineReplaced is a machine that was lost and replaced
	// with an empty one, whose files are gone (spec 009).
	TranscriptMachineReplaced Name = "transcript/machine-replaced-v1"
	// TranscriptMachineRebuilt takes Files, Paths, More, Unverified,
	// UnverifiedMore, Omitted, OmittedMore and Commands: a new machine
	// whose working directory was rebuilt from the session's log, with
	// the files that are back, the ones that may be out of date, the ones
	// that are missing, each list followed by how many more it leaves
	// unnamed, and how many commands ran since the first file (spec 065).
	TranscriptMachineRebuilt Name = "transcript/machine-rebuilt-v1"
	// TranscriptMachineWorkRestored takes Copied, Turn, Inherited, Files,
	// Paths, More, Unverified, UnverifiedMore, Omitted, OmittedMore,
	// Skipped, SkippedMore, LeftOut, MaxFileMiB and Commands: a new
	// machine whose working directory was restored from the copy the
	// session keeps in its folder, saved at the end of turn Turn or, with
	// Inherited, by the session it continues, Copied files, then brought
	// up to date from the log, with the files the log wrote, the ones that
	// may be out of date, the ones that are missing and the ones the copy
	// left out, each list followed by how many more it leaves unnamed, the
	// directories the copy leaves out, its bound on one file, and how many
	// commands ran since it was saved (spec 073).
	TranscriptMachineWorkRestored Name = "transcript/machine-work-restored-v1"
)

// The results the harness gives a call it does not run, or whose run
// did not finish (specs 005 and 012).
const (
	// CallDenied takes the person's Note, empty for none.
	CallDenied Name = "results/call/denied-v1"
	// CallUnattended is a call that would wait for a person's
	// confirmation in a session no person attends: a run of a rollout
	// (spec 077).
	CallUnattended Name = "results/call/unattended-v1"
	// CallUnknownTool takes Name.
	CallUnknownTool   Name = "results/call/unknown-tool-v1"
	CallUnknownEffect Name = "results/call/unknown-effect-v1"
	CallCanceled      Name = "results/call/canceled-v1"
	// CallFailed takes the tool's Error.
	CallFailed Name = "results/call/failed-v1"
	// CallMachineUnavailable takes the Error of a machine opened on
	// demand that could not be had (spec 009).
	CallMachineUnavailable Name = "results/call/machine-unavailable-v1"
	// CallMachineWaiting takes Approval, whether the person's computer
	// waits for its owner to allow the session or is offline: a call on a
	// device that did not run (spec 080).
	CallMachineWaiting Name = "results/call/machine-waiting-v1"
	// CallMachineAway is a call on a device whose computer stopped
	// answering while it ran (spec 080).
	CallMachineAway Name = "results/call/machine-away-v1"
	// CallMachineRefused takes Detail, the computer's own words: a call on
	// a device whose owner did not allow the session (spec 080).
	CallMachineRefused Name = "results/call/machine-refused-v1"
	// CallNotConnected takes the Title of a connection the connection
	// service answered is not connected, and its Reason (spec 069).
	CallNotConnected Name = "results/call/not-connected-v1"
	// CallKeptApart takes the Title of a connection the connection service
	// refused for this conversation alone, with the reason
	// kept_apart_from_google: the conversation already used the kind of
	// connection the installation keeps apart from it (spec 069).
	CallKeptApart Name = "results/call/kept-apart-v1"
	// CallInputRequired takes the Title of a connection whose server asked
	// for input a session cannot give (spec 069).
	CallInputRequired Name = "results/call/input-required-v1"
	// CallNoContent is a server's result with no content (spec 069).
	CallNoContent Name = "results/call/no-content-v1"
	// CallPlanMode and CallAboveBlock are the reasons of a blocked call,
	// recorded on its agent.tool_use and read by the model as its result.
	CallPlanMode   Name = "results/call/plan-mode-v1"
	CallAboveBlock Name = "results/call/above-block-v1"
	// CallOutsideNetwork takes Host: a fetch of a host outside the
	// session's network that nobody is asked about (spec 052).
	CallOutsideNetwork Name = "results/call/outside-network-v1"
	// CallNetworkUnavailable takes Host and Error: an allowed fetch whose
	// widening of the session's network was refused (spec 052).
	CallNetworkUnavailable Name = "results/call/network-unavailable-v1"
	// CallRepeated follows the error a call got when the same call, with
	// the same input, got the same error earlier in its thread's turn
	// (spec 005).
	CallRepeated Name = "results/call/repeated-v1"
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
	// ReadDocument takes Path and Kind, the document's extension without
	// its dot: the line that opens the text of an office document read
	// reads (spec 079). ReadSheet takes a sheet's Name and ReadSlide a
	// slide's Number, each the line that opens its part of the text.
	ReadDocument Name = "results/read/document-v1"
	ReadSheet    Name = "results/read/sheet-v1"
	ReadSlide    Name = "results/read/slide-v1"
	// ReadPDF takes Path, Count (its pages, counted), More, Failed and
	// Shown: the line that opens a PDF's text a parsing service read
	// (spec 082). ReadMorePages takes Shown, Pages and Next, the line that
	// ends it when more pages follow. ReadPDFUnread takes Why (kept,
	// large, timeout, refused, failed or unavailable), Max, Timeout,
	// Message and Code: the sentence after the binary answer of a PDF the
	// service did not read. ReadPagesPastEnd takes Path, Count, More and
	// Selection, ReadPagesTooLarge Path, Selection and Max.
	ReadPDF           Name = "results/read/pdf-v1"
	ReadMorePages     Name = "results/read/more-pages-v1"
	ReadPDFUnread     Name = "results/read/pdf-unread-v1"
	ReadPagesPastEnd  Name = "results/read/pages-past-end-v1"
	ReadPagesTooLarge Name = "results/read/pages-too-large-v1"

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
	// BashMachineStopped takes OutOfMemory, Restarted, Lost and Error: a
	// command that did not finish because its machine stopped under it,
	// and where the machine stands, running again with its files, lost,
	// or not running yet for the reason Error gives (spec 009).
	BashMachineStopped Name = "results/bash/machine-stopped-v1"
	// BashKilled takes Code, the exit code of a kill: a command that was
	// killed on a machine that runs on, for a cause nothing names.
	BashKilled Name = "results/bash/killed-v1"
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
// skills index and a memory store, the lines that mark instructions cut
// at their limits, and the date every request states.
const (
	// ContextBlock takes Workdir, OS, Arch, Cella, Environment, Git,
	// Branch, Head, Modified, Untracked and Commits. Version 2 drops the
	// Date line of version 1, the day the machine attached, which a
	// session.machine recorded with version 1 still carries: ContextDate
	// states the date.
	ContextBlock Name = "context/context-v2"
	// ContextDate takes Date, the UTC day of the event that opened the
	// request's turn, and Attached, the day a context block recorded with
	// version 1 names when it is another day, empty otherwise: the date a
	// request states as today's, in its last system block.
	ContextDate Name = "context/date-v1"
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
	// ContextRoute takes Route, the routed name a session runs by (spec
	// 043).
	ContextRoute Name = "context/route-v1"
	// ContextInitiator takes Text, the initiator's standing instructions
	// (spec 053).
	ContextInitiator Name = "context/initiator-v1"
	// ContextWoken leads the message that began a turn nobody waits for,
	// a wake's or a judged trigger's (spec 086): Wake, whether the agent's
	// own wake began it, and Attended, whether a question waits for the
	// person.
	ContextWoken Name = "context/woken-v1"
	// ContextAttached takes First, whether the part is the first one,
	// which opens with what the parts are, Title and Text: one context
	// part an allow of the session's create attached (spec 058).
	ContextAttached Name = "context/attached-v1"
	// ContextAttachments takes Branch, the session's branch, and Apps,
	// each with Dir, Name, URL and Slug: the apps whose source an allow
	// attached (spec 058).
	ContextAttachments Name = "context/attachments-v1"
	// ContextConnections takes Branch, the session's branch, Token, the
	// variable of the connection service's placeholder, HTTP, each with
	// Title, Account, APIURL, APIExample, GitURL, Env and Notes, MCP, each with Title,
	// Account, Name and Reason, why the connection is not connected, empty
	// when it is, and Available, each with Title and Name, the connections
	// the person may be asked to connect: the connections an allow named
	// for the session (spec 069). Version 2 adds the connections the person
	// may be asked to connect. Version 3 says the connected ones can be
	// used now with no sign-in or key, and names each one's account and,
	// for an http connection, the service's own API base URL its variable
	// stands in for, that the whole documented path goes after it, and a
	// curl call with the header, the variable and the example path; and it
	// says an error other than connection_not_connected is about the
	// request. Version 4 adds each http connection's notes, after its
	// example call, from Notes beside Env. Version 5 says a 409 whose
	// reason is kept_apart_from_google is no ask to connect again, but a
	// service this conversation cannot use and a new one can.
	ContextConnections Name = "context/connections-v5"
)
