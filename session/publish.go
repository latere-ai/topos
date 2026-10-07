// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

// ToolPublish is the name of the tool that publishes a folder of the
// session's machine as the session's own app at the installation's app
// host, or an attached app from its checkout, and releases it (specs 043
// and 059).
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
	// Folder is the folder of the machine whose files were published,
	// relative to the working directory when it is inside it: an
	// attached app's checkout, or the folder a call published to the
	// thread's own app, which a later release that names no path
	// publishes again (spec 059). Empty on a result that pushed nothing.
	Folder string `json:"folder,omitempty"`
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
	// Attached is true when the app is one an allow attached to the
	// session, published from its checkout (spec 059); a result without
	// it names an app the thread created, its own.
	Attached bool `json:"attached,omitempty"`
}

// The codes the publish tool refuses a call with before or instead of a
// request to the app host (spec 059).
const (
	// PublishAppNotAttached is an app the session was not attached and the
	// thread did not create.
	PublishAppNotAttached = "app_not_attached"
	// PublishAppRequired is a release that names neither an app nor a
	// folder while the thread's previews, or its results, name several
	// apps, or name none in a session attached apps.
	PublishAppRequired = "app_required"
	// PublishNotInCheckout is a folder outside the checkout of the
	// attached app the call names.
	PublishNotInCheckout = "not_in_checkout"
	// PublishBehindLive is a release whose commit does not hold the
	// commit the app serves.
	PublishBehindLive = "behind_live"
)

// PublishError is the app host's code and sentence.
type PublishError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}
