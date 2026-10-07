Nothing was released: the build of {{.Path}} failed: {{.Code}}{{if .Message}}: {{.Message}}{{else}}.{{end}}
{{if .Log}}The end of the build log:
{{.Log}}
{{end}}Fix the cause, then call publish with release set to true again. If it is not something you can fix, such as a project that needs a server process, tell the person plainly.
