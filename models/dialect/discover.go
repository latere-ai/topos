// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"latere.ai/x/topos/models"
)

// maxDiscovery bounds how much of a discovery answer is read.
const maxDiscovery = 1 << 20

// Discover asks base for Lux's discovery document, GET
// <base>/.well-known/lux. A Lux installation answers with the doors it
// serves. Any other answer, another status or a body that is not Lux's
// document, is a base that is not a Lux root, and Discover returns no
// doors and no error, so base is one provider's API base. A base that
// already names a door, and a scripted connection, are not asked. The
// error is a base that did not answer at all.
func Discover(ctx context.Context, client *http.Client, base string) (models.Doors, error) {
	base = strings.TrimRight(base, "/")
	if models.NamesADoor(base) || strings.HasPrefix(base, models.SchemeScripted+":") {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/lux", nil)
	if err != nil {
		return nil, fmt.Errorf("models: discover %s: %w", base, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: discover %s: %w", base, err)
	}
	var doc struct {
		Name  string            `json:"name"`
		Doors map[string]string `json:"doors"`
	}
	read := resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, maxDiscovery)).Decode(&doc) == nil
	if err := resp.Body.Close(); err != nil {
		return nil, fmt.Errorf("models: discover %s: %w", base, err)
	}
	if !read || doc.Name != "lux" || len(doc.Doors) == 0 {
		return nil, nil
	}
	return models.Doors(doc.Doors), nil
}
