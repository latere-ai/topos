The person answered.
{{range .Questions}}
{{.Number}}. {{.Header}}: {{.Question}}{{if .Chosen}}
   Chosen: {{.Chosen}}{{end}}{{if .Text}}
   In their words: {{.Text}}{{end}}{{if .Left}}
   Left to you. Decide, and state what you assumed.{{end}}{{end}}
