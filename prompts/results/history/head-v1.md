{{- if eq .Kind "message"}}{{or .Name .Subject "someone"}} wrote:
{{- else if eq .Kind "agent"}}You wrote:
{{- else if eq .Kind "call"}}You called {{.Tool}}, id {{.ID}}:
{{- else if eq .Kind "result"}}{{or .Tool "The call"}} returned {{.Outcome}}, id {{.ID}}:
{{- else if eq .Kind "answer"}}{{or .Name .Subject "someone"}} answered your question, id {{.ID}}:
{{- else if eq .Kind "interrupt"}}{{or .Name .Subject "someone"}} interrupted you.
{{- else if eq .Kind "title"}}{{or .Name .Subject "someone"}} changed the conversation's title to:
{{- else if eq .Kind "task"}}The thread that started you gave you this task:
{{- else}}This event was removed from the log; nothing of what it held is shown.
{{- end}}
