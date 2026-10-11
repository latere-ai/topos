// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"mime"
	"net/http"
	"path"
)

// MediaType is a file's media type by the one rule the files route
// answers a file with and files.kept records a linked file with (specs
// 044 and 064): the type its name's extension names, and otherwise the
// one its leading bytes give, of which head holds at most
// tools.SniffBytes. A file neither names is application/octet-stream,
// and one with no bytes text/plain. The content sniffer is net/http's,
// which the harness tree may not reach (spec 001), so a drive hands this
// function to its harness as Config.MediaType.
func MediaType(name string, head []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(head)
}
