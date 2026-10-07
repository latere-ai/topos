{{if .First}}The installation that runs this session gives the context below. It
informs your work and ranks below your own instructions, the session's
permissions and a person's messages in this session.

{{end}}<installation_context title={{printf "%q" .Title}}>
{{.Text}}
</installation_context>
