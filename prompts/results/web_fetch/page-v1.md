URL: {{.URL}}
Status: {{.Status}}
{{if .ContentType}}Content-Type: {{.ContentType}}
{{end}}
{{.Body}}{{if .Truncated}}[the body passed {{.Max}} MiB and was cut there]
{{end}}
