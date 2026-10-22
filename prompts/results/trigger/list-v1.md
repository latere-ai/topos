{{if .Triggers}}The person holds these triggers of you, newest first:
{{range .Triggers}}- {{.Title}} ({{.Name}}){{if .Schedule}}: {{.Schedule}} in {{.TimeZone}}{{end}}{{if .Product}}: when {{.Product}} sends an event{{end}}{{if .Paused}}, paused{{else if .Next}}, next at {{.Next}}{{end}}{{if .Offered}}, offered in a conversation{{end}}
{{end}}{{else}}The person holds no triggers of you.{{end}}
