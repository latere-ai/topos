The release was not made: {{.App}} is live at {{.Live}}, which this work does not include. Another session probably released it.
{{if .Attached}}Merge it into {{.Dir}}/ first:
  git -C {{.Dir}} fetch origin --tags
  git -C {{.Dir}} merge {{.Live}}
{{else}}Merge it into the folder you published first, through the session's git directory for the app, where <folder> is that folder:
  git --git-dir="$HOME/.topos/publish/{{.App}}.git" --work-tree=<folder> fetch {{.URL}} --tags
  git --git-dir="$HOME/.topos/publish/{{.App}}.git" --work-tree=<folder> merge {{.Live}}
{{end}}Resolve any conflict and check the result, then call publish with app "{{.App}}" and release set to true again: that one call publishes the merge and releases it.
