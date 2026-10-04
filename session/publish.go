// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

// ToolPublish is the name of the tool that publishes a folder of the
// session's machine as the session's own app at the installation's app
// host, and releases it (spec 043).
const ToolPublish = "publish"

// The statuses a publish result records (spec 043): a preview's, and a
// release's.
const (
	PublishReady    = "ready"
	PublishBuilding = "building"
	PublishFailed   = "failed"
	PublishCanceled = "canceled"
	PublishReleased = "released"
	PublishPending  = "pending"
	PublishRefused  = "refused"
)

// PublishMeta is what the tool.result of a publish call records in its
// meta under "publish": the app, the commit, and where its preview or
// its release stands. The thread's state folds it, and a client reads it
// to show the preview and the release.
type PublishMeta struct {
	// App is the app's slug at the app host, and Name its name.
	App  string `json:"app"`
	Name string `json:"name,omitempty"`
	// URL is the app's public address.
	URL string `json:"url,omitempty"`
	// Preview is the preview deploy's own address, once the host names
	// it.
	Preview string `json:"preview,omitempty"`
	// Commit is the commit published or released.
	Commit string `json:"commit,omitempty"`
	// Deploy is the id of the deploy that carries it.
	Deploy string `json:"deploy,omitempty"`
	// Status is one of the Publish statuses, empty for a result that
	// names the app alone.
	Status string `json:"status,omitempty"`
	// Release is the tag of a release.
	Release string `json:"release,omitempty"`
	// Error is the host's code and sentence for a failed, canceled or
	// refused deploy or release.
	Error *PublishError `json:"error,omitempty"`
}

// PublishError is the app host's code and sentence.
type PublishError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}
