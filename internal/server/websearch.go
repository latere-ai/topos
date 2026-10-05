// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/harness/tools"
)

// webSearchResultMeta is the WebSearchResultMeta schema (spec 047): what
// the tool.result of a web_search call records beside its text, which a
// client reads to show a refused search or what a search cost.
var webSearchResultMeta = yaml.MapSlice{
	{Key: "type", Value: "object"},
	{Key: "description", Value: fmt.Sprintf("The meta of a %s call's tool.result. An agent holds %s only when its manifest names it, and each call sends its query to the search service the installation configures, with the session's own credential. "+
		"The result's outcome is %s with the results, or with none; %s when the service refused the search, the text then the service's own sentence for the person and refusal its code, or when the search failed; %s past the search's timeout. "+
		"A search the service charged for carries cost_usd_micro on the tool.result itself, beside meta, and the session's spent_cost_usd_micro counts it as it counts a model request's.",
		tools.NameWebSearch, tools.NameWebSearch, tools.OutcomeOK, tools.OutcomeError, tools.OutcomeTimeout)},
	{Key: "properties", Value: yaml.MapSlice{
		{Key: "refusal", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The code the search service refused the search with; absent on a search it answered."}}},
	}},
}
