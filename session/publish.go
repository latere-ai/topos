// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

// ToolPublish is the name of the tool that publishes a folder of the
// session's machine as the session's own app at the installation's app
// host, or an attached app from its checkout, releases it, and changes
// who can open it (specs 043 and 059).
const ToolPublish = "publish"

// The values of the publish tool's access input (spec 043): who can open
// an app once the call ran.
const (
	// PublishAccessLink is anyone who has the app's address, without
	// signing in.
	PublishAccessLink = "link"
	// PublishAccessPrivate is the person and the people they share the
	// app with.
	PublishAccessPrivate = "private"
)

// The visibilities of an app as the app host names them (spec 043): what
// a result's Visibility reads, and of them the two a call with access
// writes, unlisted for link and private for private. The tool never
// writes public.
const (
	VisibilityPrivate  = "private"
	VisibilityUnlisted = "unlisted"
	VisibilityPublic   = "public"
)

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
// to show the preview and the release. The result of a call with access
// names the app, its address and its visibility after the call, with no
// status, commit or deploy: it published nothing.
type PublishMeta struct {
	// App is the app's slug at the app host, and Name its name, as the
	// host answered them when the call ran.
	App  string `json:"app"`
	Name string `json:"name,omitempty"`
	// AppID is the app's id at the app host, which its owner's rename of
	// the app leaves as it is while App changes, so a later call finds
	// the app by it (spec 071). Empty where the host named none, and on a
	// result recorded before results carried it.
	AppID string `json:"app_id,omitempty"`
	// URL is the app's own address.
	URL string `json:"url,omitempty"`
	// Visibility is who may open the app, as the app host named it when
	// the call ran: private, unlisted or public at a host that speaks
	// those words. Empty where the host names none. On the result of a
	// call with access it is the host's answer to the change, or what the
	// app had when the host refused it. A client shows it.
	Visibility string `json:"visibility,omitempty"`
	// Preview is the preview deploy's own address, once the host names
	// it.
	Preview string `json:"preview,omitempty"`
	// Folder is the folder of the machine whose files were published,
	// relative to the working directory when it is inside it: an
	// attached app's checkout, or the folder a call published to the
	// thread's own app, which a later release that names no path
	// publishes again (spec 059). The result of a call with access
	// repeats the folder of the thread's last result for the app, so the
	// thread's state keeps it. Empty on any other result that pushed
	// nothing.
	Folder string `json:"folder,omitempty"`
	// Commit is the commit published or released.
	Commit string `json:"commit,omitempty"`
	// Deploy is the id of the deploy that carries it.
	Deploy string `json:"deploy,omitempty"`
	// Status is one of the Publish statuses, empty for a result that
	// names the app alone, as that of a call with access does.
	Status string `json:"status,omitempty"`
	// Release is the tag of a release.
	Release string `json:"release,omitempty"`
	// Error is the host's code and sentence for a failed, canceled or
	// refused deploy or release, and for a change of who can open the app
	// that the host refused.
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
	// apps, or name none in a session attached apps; and a call with
	// access that names no app while they name several, or while the
	// thread has published none.
	PublishAppRequired = "app_required"
	// PublishAccessAlone is a call with access that also names a folder
	// or asks for a release: a change of who can open an app is a call of
	// its own (spec 043).
	PublishAccessAlone = "access_alone"
	// PublishNotInCheckout is a folder outside the checkout of the
	// attached app the call names.
	PublishNotInCheckout = "not_in_checkout"
	// PublishBehindLive is a release whose commit does not hold the
	// commit the app serves.
	PublishBehindLive = "behind_live"
)

// PublishRoleInsufficient is the reason an app host names when it refuses
// a change of who can open an app because the app belongs to an
// organization and the caller's role there does not reach its settings.
const PublishRoleInsufficient = "role_insufficient"

// PublishError is the app host's code and sentence, and the reason its
// refusal names beside them when it names one: a token that says why,
// such as PublishRoleInsufficient.
type PublishError struct {
	Code    string `json:"code"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}
