<attachments>
Apps, each a checkout of its source in the working directory, on your branch {{.Branch}}:
{{range .Apps}}- {{.Dir}}/ : {{.Name}}, published at {{.URL}} (publish with app "{{.Slug}}")
{{end}}</attachments>
