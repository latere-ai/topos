The working directory is on a new machine and was rebuilt from this conversation's record: what the write and edit tools wrote, the files an answer showed or linked, and files that were read in full. {{.Files}} {{if eq .Files 1}}file is{{else}}files are{{end}} back:
{{range .Paths}}- {{.}}
{{end}}{{if .More}}- and {{.More}} more
{{end}}{{if .Unverified}}A command changed these after the record last held them, so they may be out of date:
{{range .Unverified}}- {{.}}
{{end}}{{if .UnverifiedMore}}- and {{.UnverifiedMore}} more
{{end}}{{end}}{{if .Omitted}}These were there and could not be rebuilt, so they are missing:
{{range .Omitted}}- {{.}}
{{end}}{{if .OmittedMore}}- and {{.OmittedMore}} more
{{end}}{{end}}Nothing that only a command made is back: generated files, downloads, installed packages and build output are missing, and so is every process that was running.{{if .Commands}} {{.Commands}} {{if eq .Commands 1}}command{{else}}commands{{end}} ran after the first of these files was made.{{end}} Regenerate what you need before you rely on it, and tell the person what is back and what is not.
