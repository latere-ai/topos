// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// DeviceRule is the sentence the create route states a device machine by
// (spec 080): how a create names a person's own computer, what the
// authorizer is asked, and what the session's machine is and does not do
// there.
var DeviceRule = fmt.Sprintf("The body's machine names a person's own computer for the session to work on, {kind: %s, environment, workdir}: environment the name of the Cella environment the computer serves, a DNS label, "+
	"and workdir the folder on it the session works in, an absolute, clean path of at most %d bytes with no control character. Any other kind, and any other member, is invalid_request, and a server without Cella is machine_unavailable. "+
	"The session records it as its machine for its whole life. session.create carries machine %s with environment and workdir, so the authorizer decides whether the person the session acts for may use that computer; "+
	"a fork of the session carries the same three in session.fork, decided for the person forking, and a fork the authorizer refuses is not created. "+
	"A trigger names a device the same way in spec.session.machine, the one machine a trigger names; its trigger.create and trigger.update carry machine %s with environment and workdir, "+
	"so the authorizer decides whether the person applying it may use that computer when it is applied, as well as at each firing's session.create, and the sessions a trigger starts record trigger_fires_on, %s or %s. "+
	"The session's sandbox is created on that environment with the folder bound in place as its workspace and working directory, with no image, no Secret, no LUX_URL and no HOME, "+
	"labeled %s and, for a session a trigger started, %s and %s, and annotated %s with the agent's display name, which the computer shows its owner. "+
	"No repository is delivered into the folder, the files of a message are written under %s/ in the sandbox's temporary directory, and the session keeps no checkpoint and no copy of its working directory and restores neither, "+
	"so a rewind finds no checkpoint. A call there scores as a call on a host and is decided in confirm mode when the session runs progressive, by the agent's manifest or a change of the mode: "+
	"a call that reads runs, and any other asks unless always_allow names it or the person allowed it earlier in the session with remember, plan still blocks a change, and each agent.tool_use records mode confirm. "+
	"The model's requests carry prompt_version %s.",
	session.MachineDevice, session.MaxDeviceWorkdir, session.MachineDevice, session.MachineDevice, session.TriggerSchedule, session.TriggerEvents,
	cella.LabelAgentVersion, cella.LabelTrigger, cella.LabelTriggerFiresOn, cella.AnnotationAgentName, cella.DeviceDir+"/"+session.AttachmentDir, prompts.HarnessVersion(prompts.HarnessDevice))

// DeviceWaitRule is the sentence the event routes state a session waiting
// for a person's computer by (spec 080).
var DeviceWaitRule = fmt.Sprintf("A session on a person's own computer stops with stop_reason %s, the reason, %s or %s, in its session.status detail, when the computer is offline or asleep, "+
	"or when its owner has not allowed the session within %s of a call's asking, the create left waiting on the computer; "+
	"session.machine_waiting {reason, environment} names the reason, appended when a call begins to wait for the owner, which a client shows as a request to allow the session on the computer, "+
	"and when a turn stops for a computer that is offline, so the last one names why the session waits. A computer that stops answering under a call stops the turn the same way, reason %s. "+
	"A computer whose owner refused the session, or did not answer in time, appends session.error {code: %s, message: %q, detail}, detail the computer's own words; "+
	"the call is answered so, and so is every later call of the turn that acts on the machine, and the turn goes on. "+
	"The next user.message, or a resume, continues a session idle on %s on the same computer.",
	session.StopMachineWaiting, session.MachineOffline, session.MachineApproval, spoken(cella.DefaultApprovalWait), session.MachineOffline,
	machine.CodeRefused, harness.MessageMachineRefused, session.StopMachineWaiting)
