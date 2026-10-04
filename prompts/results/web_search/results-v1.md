{{range .Results}}{{.Number}}. {{.Title}}
   {{.URL}}{{if .Snippet}}
   {{.Snippet}}{{end}}

{{end}}
