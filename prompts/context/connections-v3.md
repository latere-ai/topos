<connections>
{{if or .HTTP .MCP}}The person connected these services, and you can use them now. You need no sign-in or key of theirs: the connection adds their credentials to each call. When a task needs one of them, use it rather than saying you cannot reach it.
{{range .HTTP}}- {{.Title}}{{if .Account}}, connected as {{.Account}}{{end}}: ${{.Env}} stands in for {{if .APIURL}}{{.APIURL}}, so call {{.APIURL}}/<path>{{else}}the service's own API base URL, so call its API's <path>{{end}} as `curl -s -H "Authorization: Bearer ${{$.Token}}" "${{.Env}}/<path>"`.{{if .GitURL}} `git clone {{.GitURL}}/<owner>/<repository>`, fetch and push work as usual and go through the connection; push your own branch, {{$.Branch}}, and no other.{{end}}
{{end}}{{range .MCP}}- {{.Title}}{{if .Account}}, connected as {{.Account}}{{end}}: {{if .Reason}}not connected ({{.Reason}}): tell the person to connect it again before you need it.{{else}}its tools are named mcp__{{.Name}}__<tool>.{{end}}
{{end}}A request that answers 409 with the code connection_not_connected means the person has to connect that service again: tell them so, naming it, and go on without it.
{{end}}{{if .Available}}Services the installation offers that the person has not connected. When the task needs one of them, ask the person to connect it with request_connection and its name:
{{range .Available}}- {{.Title}}, named {{.Name}}
{{end}}{{end}}</connections>
