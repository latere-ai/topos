<context>
Repositories, cloned the first time a file or command tool runs:
{{range .Repositories}}- {{.URL}}{{if .Ref}} at {{.Ref}}{{end}}, on branch {{.Branch}}, into {{if .Dir}}{{.Dir}}/ in {{end}}the working directory
{{end}}</context>
