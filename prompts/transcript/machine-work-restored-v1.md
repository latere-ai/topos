The working directory is on a new machine and was restored from the copy this conversation keeps of it, {{if .Inherited}}as the conversation this one continues last saved it, so it may hold changes made after the point this one continues from{{else}}as it was saved at the end of turn {{.Turn}}{{end}}{{if .Copied}}: {{.Copied}} {{if eq .Copied 1}}file{{else}}files{{end}}{{else}}, which held no files{{end}}.
{{if .Files}}Then {{if eq .Files 1}}this was{{else}}these were{{end}} brought up to date from this conversation's record, from what the write and edit tools wrote, the files an answer showed or linked, and files that were read in full after that:
{{range .Paths}}- {{.}}
{{end}}{{if .More}}- and {{.More}} more
{{end}}{{end}}{{if .Unverified}}A command changed these after the record last held them, so they may be out of date:
{{range .Unverified}}- {{.}}
{{end}}{{if .UnverifiedMore}}- and {{.UnverifiedMore}} more
{{end}}{{end}}{{if .Omitted}}These were there and could not be restored, so they are missing:
{{range .Omitted}}- {{.}}
{{end}}{{if .OmittedMore}}- and {{.OmittedMore}} more
{{end}}{{end}}{{if .Skipped}}These were too large for the copy, or past what it holds, so they are missing:
{{range .Skipped}}- {{.}}
{{end}}{{if .SkippedMore}}- and {{.SkippedMore}} more
{{end}}{{end}}The copy does not keep installed dependencies and caches ({{.LeftOut}}), the history of any repository (.git), files larger than {{.MaxFileMiB}} MiB, or running processes.{{if .Commands}} {{.Commands}} {{if eq .Commands 1}}command{{else}}commands{{end}} ran after the copy was saved; what {{if eq .Commands 1}}it{{else}}they{{end}} made or changed since is missing.{{end}} Reinstall dependencies from the project's manifests, such as package.json, requirements.txt or pyproject.toml, before you run anything that needs them, and tell the person what is back and what is not.
