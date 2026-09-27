// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package report renders a shop's settings for a person to read.
package report

import (
	"fmt"

	"example.com/shop/kv"
)

// Summary is one line naming the shop and its port.
func Summary(settings map[string]string) string {
	name, ok := kv.Fetch(settings, "name")
	if !ok {
		name = "unnamed"
	}
	return fmt.Sprintf("%s on port %s", name, kv.FetchOr(settings, "port", "80"))
}

// Owner is the shop's owner, fetched from the settings.
func Owner(settings map[string]string) string {
	owner, _ := kv.Fetch(settings, "owner")
	return owner
}
