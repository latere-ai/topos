<context>
Working directory: {{.Workdir}}
Platform: {{.OS}}/{{.Arch}}
Machine: {{if .Cella}}Cella sandbox{{if .Environment}} ({{.Environment}}){{end}}{{else}}host{{end}}
Date: {{.Date}}
{{if .Git}}Git: branch {{.Branch}} at {{.Head}}, {{.Modified}} modified, {{.Untracked}} untracked
{{if .Commits}}Recent commits:
{{range .Commits}}- {{.}}
{{end}}{{end}}{{end}}</context>
