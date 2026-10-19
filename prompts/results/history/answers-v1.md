{{range .Answers}}{{.Number}}. {{if .Left}}Left to you.{{else}}{{with .Chosen}}Chosen: {{.}}{{end}}{{if and .Chosen .Text}}. {{end}}{{with .Text}}In their words: {{.}}{{end}}{{end}}
{{end}}
