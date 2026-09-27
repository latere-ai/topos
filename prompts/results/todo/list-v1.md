The todo list, {{.Done}} of {{.Total}} completed:
{{range .Items}}[{{.Status}}] {{.ID}}: {{.Content}}
{{end}}
