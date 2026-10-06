# Appointment Reminders (Email and SMS)

> **Status:** in progress (branch `sms-notif`). PR 1 (system actor) is implemented; PRs 2 to 4
> are planned. Last updated 2026-10-05.

LibreDental will send automatic appointment reminders to patients by email and text message.
This document records the design decisions, the research behind them, and how the feature
fits into the existing notification framework from #89.

## Design decisions

**No project-hosted infrastructure.** LibreDental promises that installs never depend on
third-party cloud servers run by the project. Each practice brings its own email and SMS
vendor accounts. The app calls the vendor APIs directly from the practice's machine, with
credentials stored in the OS keychain (`SecretsService`). This also keeps the project out of
the path of patient data, so no Business Associate Agreement (BAA) is needed between
LibreDental and each practice.

**First vendors:**

| Channel | Vendor | Why |
| --- | --- | --- |
| Email | Any SMTP server (Amazon SES SMTP, Google Workspace, Microsoft 365, Mailgun, ...) | One provider covers most hosts; the Go standard library is enough. |
| SMS | AWS End User Messaging SMS | AWS signs a BAA self-serve and at no cost through AWS Artifact, and the service is on AWS's HIPAA-eligible list. Twilio only offers a BAA on its paid Security and Enterprise Editions. |

The provider registry supports several vendors per channel, so a Twilio provider can be
added later for practices that prefer it.

**Optional, practice-owned IaC.** A Terraform module (planned, see [PR 4](#pr-4-optional-terraform-module)) will set up
the AWS side in the *practice's own* account: SES domain identity and DKIM, SMS resources, and
a least-privilege IAM user. The project runs none of it.

## What exists today

From #89:

| Piece | Location |
| --- | --- |
| `NotificationProvider` interface (email, SMS, voice) | `internal/domain/notification.go` |
| Provider registry, keychain config, manual `SendNotification` | `internal/services/notification_service.go` |
| Delivery history table `notification_log` | `internal/storage/sqlite/schema/notifications.sql` |
| Patient opt-in flag `reminder_opt_in` | `internal/domain/patient.go` |
| Provider config panel | `frontend/src/views/clinic/IntegrationsSection.svelte` |

`SendNotification` already checks the patient's opt-in, checks that the appointment belongs
to the patient, and records every attempt (success or failure) in both `notification_log`
and the audit trail.

**Missing:** no provider is registered in `main.go`, nothing sends reminders automatically,
there are no reminder settings or templates, and `practice_config` has no timezone.

## The system actor

### Why it's needed

Every audit entry today is tied to a logged-in staff member. `AuditService.LogAction` and
`LogPatientAction` look up the session token and return `ErrUnauthorized` without one.
Automatic reminders are sent by a background job with nobody logged in, so they can't be
audited under the current API. Skipping the audit entry is not an option (see `AGENTS.md`),
and attributing the send to whichever staff member last logged in would be false.

The audit trail needs a **system actor**: a reserved, non-human identity that records actions
the software takes on its own.

### What the standards say

- **HIPAA Security Rule, 45 CFR 164.312(b) (audit controls):** systems holding ePHI must
  record and examine activity. Sending a patient's contact details and appointment time to a
  vendor is activity on ePHI, whoever starts it.
- **45 CFR 164.312(a)(2)(i) (unique user identification):** each identity must be unique and
  traceable. A system actor must never share an ID with a staff account, and it must never be
  a shared login that people could use.
- **NIST SP 800-53 AU-3 (content of audit records):** a record must show what happened, when,
  where, the source, the outcome, and the identity of the individuals, *subjects, or
  processes* involved. That explicitly covers non-human actors, and it means failures must be
  logged as well as successes.
- **HL7 FHIR `AuditEvent`:** the healthcare-standard audit model lets an agent be a person,
  device, or *software*, and marks which agent was the `requestor` (the initiator). LibreDental
  doesn't use FHIR, but it's a useful model: an automatic reminder's requestor is the
  software, and the staff member who turned reminders on appears in a separate entry for that
  configuration change.

### Proposed design

1. **Reserved IDs with a prefix.** System actors use IDs of the form `system:<job>`, for
   example `system:reminders`. One ID per background job means the audit log shows *which*
   process acted, as AU-3 asks. The constants live in `internal/domain/audit.go`.
2. **No audit schema change.** `audit_logs.user_id` is free text in a separate database, with
   no foreign key to `providers`, so system IDs fit the existing columns and need no migration.
3. **Not callable from the frontend.** Wails binds every exported method on a registered
   service, and in LAN server mode that means any client on the network can call it. A method
   like `AuditService.LogSystemAction` would let any client write audit entries that look like
   they came from the system. The system-logging entry point must be unexported, or a
   package-level function, following the `RegisterNotificationProvider` pattern.
4. **Staff can't take a system ID.** `PracticeConfigService.SaveProvider` and
   `CreateInitialProvider` accept a client-supplied provider ID, and the repository saves with
   `ON CONFLICT(id) DO UPDATE`. Today a client could create a staff account with ID
   `system:reminders`, log in with its PIN, and have its actions look automatic. Both methods
   must reject IDs starting with `system:`. Generated IDs are always `prov_<nanoseconds>`, so
   this should only ever reject a deliberately crafted ID. Before shipping, check that no
   existing install could have such an ID.
5. **Tie automatic actions to a human decision.** Turning reminders on or changing their
   settings is audited under the staff member who did it. Each system entry's details name the
   rule it followed (for example "48-hour SMS reminder"), so an auditor can trace a send back
   to the configuration change behind it.
6. **Localized display.** The audit view (`frontend/src/views/AuditingSubtab.svelte`) shows
   `user_name` as stored. For `system:` IDs it shows a Paraglide string ("LibreDental
   (automatic)") rather than English text stored in the database. The ID next to it, such as
   `system:reminders`, says which job acted.

A system send would produce an audit entry like this:

| Field | Value |
| --- | --- |
| `user_id` | `system:reminders` |
| `user_name` | `LibreDental` |
| `patient_id` | the patient's ID |
| `action` | `CREATE` |
| `resource` | `notification` |
| `resource_id` | the `notification_log` entry's ID (currently unused by notifications) |
| `details` | the rule, channel, provider, and outcome, for example "Sent 48-hour SMS reminder via aws_sms" or the failure reason |

### Sharing the send path

`SendNotification` is token-gated and stays that way for manual sends. Its body (opt-in
check, recipient lookup, provider call, log entry, audit entry) moves into an unexported
method that takes an *actor*: either the session user or a system actor. The manual send and
the background job then go through the same checks and the same audit logic. That stops the
two paths from drifting apart.

## Sending reminders safely

### Never send a duplicate

A reminder must go out at most once per appointment, rule, and channel, even if the app
crashes, restarts, or two processes run at once. Vendor-side deduplication shouldn't be
relied on.

The plan is **claim before send**, a variant of the transactional outbox pattern:

1. Insert a `notification_log` row with status `pending` before calling the vendor. A unique
   index on `(appointment_id, reminder_kind, channel)` makes a second claim fail, so only one
   process can send.
2. Call the vendor.
3. Update the row to `sent` or `failed`.

If the app crashes between steps 1 and 3, the row stays `pending`. On restart it isn't
retried automatically, because the message may or may not have gone out. It's shown to staff
as "status unknown". For reminders, a missed message is less harmful than a duplicate, and
duplicates also count against the TCPA limits below.

This needs an additive migration on `notification_log`: a nullable `reminder_kind` column
(NULL for manual sends) and the unique index. SQLite treats NULLs as distinct in unique
indexes, so existing rows and manual sends are unaffected. The migration is generated with
Atlas, per `AGENTS.md`.

### Check again right before sending

The job re-reads the appointment and patient just before each send, and skips the reminder
if:

- the appointment is cancelled, completed, a no-show, or has been rescheduled outside the
  reminder window;
- the patient has turned off `reminder_opt_in`;
- the patient has no email address or phone number for that channel.

### Message content (HIPAA minimum necessary)

HHS treats appointment reminders as part of treatment, so they don't need the patient's
written authorization. They must still disclose only the minimum necessary. Templates are
limited to:

- the patient's first name;
- the appointment date and time, in the practice's timezone;
- the practice's name and phone number.

Templates must **not** include the procedure, the appointment reason, the provider's
specialty, or anything else from the chart. Unencrypted SMS and email are acceptable when the
patient has been told about the risk and chose that channel. If a patient asks for
confidential communications, the practice must offer another channel.

### TCPA (US text messages)

The FCC's healthcare exemption lets providers text appointment reminders to a patient's
mobile number without prior express written consent, if the messages:

- go only to the number the patient gave the practice;
- are not marketing and are free to the patient;
- include the practice's name and contact information;
- include a way to opt out (for example "Reply STOP to opt out");
- are limited to **one per day and three per week** per patient.

The scheduler enforces the frequency limit across all of a patient's appointments. Default
reminder templates include the practice's name, phone number, and opt-out text. This is
engineering guidance, not legal advice. Practices should confirm their obligations,
including state rules.

AWS End User Messaging SMS adds a number to its opt-out list automatically when someone
replies STOP. That list is not synced back to the patient's `reminder_opt_in` flag yet. The
app can't receive webhooks, so syncing would mean polling (for example SNS to SQS) in a later
phase.

### Time and quiet hours

- `practice_config` gets a timezone column (IANA name, for example `America/Los_Angeles`).
  Appointment times are rendered in it, not in the server machine's local timezone.
- Reminders are only sent inside configurable quiet hours (default 08:00 to 21:00 in the
  practice's timezone). A reminder that falls outside them moves to the next allowed time, as
  long as that's still before the appointment.

### When the job runs

The job runs inside the Go process on a ticker (for example every 5 minutes):

- **LAN server mode:** the server is always on, so reminders go out on time.
- **Single desktop install:** reminders only go out while the app is open. At startup the
  job catches up on reminders that are still due and skips any whose appointment has already
  started. The settings screen should say this.

The unique index from [Never send a duplicate](#never-send-a-duplicate) keeps this safe even
if a desktop app and a server ever point at the same database.

## Implementation plan

Four PRs, each reviewable on its own and each leaving `main` working. Every PR ends with
`task format` and `task test` clean, and CI's `gofmt -l`, `go vet`, `go test ./internal/...`,
`format:check`, and `svelte-check` passing.

Sizes are rough estimates to help reviewers plan, not commitments.

| PR | Scope | Schema change | Est. size (code / tests) |
| --- | --- | --- | --- |
| 1 | System actor | None | ~80 / ~150 lines |
| 2 | Email and SMS providers, send test | None | ~450 / ~600 lines |
| 3 | Automatic reminders | Yes, additive | ~1,100 / ~1,000 lines |
| 4 | Optional Terraform module | None | ~250 HCL / ~80 lines |

### PR 1: System actor (implemented)

Audit-only. No migration, no new dependencies, no change for existing callers.

**Modified**

| File | Change |
| --- | --- |
| `internal/domain/audit.go` | Add `SystemActorPrefix = "system:"`, `SystemActorReminders = "system:reminders"`, and `IsSystemActorID(id string) bool`. |
| `internal/services/audit_service.go` | Add an unexported `auditActor` (ID and name) and `logPatientActionAs(actor, action, patientID, resource, resourceID, details)`. `LogPatientAction` resolves the session user and delegates to it, so existing behavior is unchanged. Add unexported `logSystemPatientAction`, which only accepts `system:` IDs. Nothing new is exported, so Wails binds nothing new. |
| `internal/services/config_service.go` | `SaveProvider` and `CreateInitialProvider` reject IDs starting with `system:` with `storage.ErrInvalidInput`. The check ignores case and surrounding spaces, so look-alikes such as `SYSTEM:reminders` are refused too. |
| `frontend/src/views/AuditingSubtab.svelte` | For `system:` user IDs, show a localized actor name instead of the stored `user_name`. |
| `frontend/messages/en.json` | New key `audit_system_actor`. |
| `docs/notification-alerts.md` | Mark PR 1 done. |

**Added:** `internal/services/audit_service_system_test.go`. It uses the internal `services`
package because `logSystemPatientAction` is unexported; the existing `audit_service_test.go`
uses the external `services_test` package.

**Tests**

| Test | Checks |
| --- | --- |
| `TestAuditService_LogSystemPatientAction` (new) | Writes an entry with no session; `user_id`, `user_name`, `patient_id`, `resource_id`, and details are stored; `""`, `prov_1`, a bare `system:`, `System:reminders`, and ` system:reminders` are rejected and write nothing. |
| `TestAuditService_BoundMethods` (new) | Uses reflection to compare `AuditService`'s exported methods against a fixed list. It fails if anyone exports a method later and accidentally makes system logging callable from the frontend. |
| `TestPracticeConfigService_RejectsReservedProviderIDs` (new) | `SaveProvider` and `CreateInitialProvider` refuse `system:` IDs, including case and spacing look-alikes, and no provider is saved; a failed onboarding attempt leaves onboarding open; IDs that only contain "system" (`prov_system`, `systemadmin`) and generated IDs still save. |
| `TestAuditService`, `TestAuditService_LogPatientActionUsesSessionUser` (existing) | Regression: session logging is unchanged. |

**Results:** `task format` and `task test` are clean, as are CI's `gofmt -l` and
`go vet ./internal/...`. Both new guard tests were checked by disabling the check they cover:
each failed, then passed again once the check was restored.

**Manual (still to do):** with `task desktop`, open Audit and check that staff entries look
the same as before. There are no system entries to see until PR 3.

### PR 2: Email and SMS providers

**New dependency:** `github.com/aws/aws-sdk-go-v2` with only the `aws`, `credentials`, and
`service/pinpointsmsvoicev2` modules. The SDK's `config` package is deliberately not used:
its default credential chain reads `~/.aws` and environment variables, which would let the app
silently send with a developer's CLI credentials. The provider only uses the keys saved in the
app.

**Modified**

| File | Change |
| --- | --- |
| `internal/services/secrets_service.go` | Redact every secret field, not only `api_key`: also `password` (SMTP) and `secret_access_key` (AWS). Restore each from the keychain when the frontend sends back the placeholder. Today an SMTP password or AWS secret would go to the frontend in plain text. |
| `internal/services/notification_service.go` | Add `SendTestMessage(token, providerName, to, subject, body)`: token-gated, sends to an address staff type in (not a patient), and audited with `LogAction`. It writes no `notification_log` row, because that table requires a patient. For SMS, `recipientFor` normalizes the patient's phone to E.164 using the practice's country, so the constructor also takes the practice config repository. |
| `main.go` | Register the two providers. Pass the practice config repository to `NewNotificationService`. |
| `frontend/src/views/clinic/IntegrationsSection.svelte` | The notification panel shows each provider's own fields instead of a single API key field. Secret fields use password inputs and keep the redaction placeholder. Add a "Send test" form. |
| `frontend/messages/en.json` | Field labels, help text, test-send messages. |
| `go.mod`, `go.sum` | AWS SDK modules. |

**Added**

| File | Contents |
| --- | --- |
| `internal/services/notification_provider_smtp.go` | `smtp_email` provider. Config: `host`, `port`, `username`, `password`, `from_address`, `from_name`, `tls_mode` (`starttls` or `implicit`). Builds an RFC 5322 message (encoded subject, `Date`, `Message-ID`, CRLF line endings). Never sends credentials without TLS. |
| `internal/services/notification_provider_aws_sms.go` | `aws_sms` provider. Config: `access_key_id`, `secret_access_key`, `region`, `origination_identity`, optional `configuration_set`. Calls `SendTextMessage` with message type `TRANSACTIONAL`. Maps AWS errors (for example a destination on the opt-out list) to readable failure messages. |
| `internal/services/notification_phone.go` | `toE164(raw, country)`: normalizes free-text phone numbers. Rejects anything it can't normalize instead of guessing. |
| `*_test.go` for each of the three files above | See below. |

**Tests**

| Test | Checks |
| --- | --- |
| SMTP message building (new) | Headers, non-ASCII subject encoding, CRLF line endings, a body line starting with `.`. |
| SMTP transport (new) | An in-process fake SMTP server with a test TLS certificate. Covers a successful send, the server rejecting a recipient, bad credentials, and the provider refusing to authenticate when the server doesn't offer TLS. |
| SMTP config (new) | Missing or invalid `host`, `port`, `from_address`, `tls_mode` are reported before connecting. |
| AWS request shape (new) | An `httptest` server is set as the SDK endpoint. The request has the right `X-Amz-Target`, a SigV4 `Authorization` header, and the expected JSON fields. |
| AWS responses (new) | Replays AWS's documented success and error responses: message ID stored; validation, throttling, and opt-out errors become failed results with a clear message. |
| AWS config (new) | Missing keys or region fail before any request. No fallback to environment or `~/.aws` credentials (checked with those variables set). |
| `toE164` table test (new) | US formats with and without `+1`, spaces, dashes, parentheses, extensions, too short or long, non-US numbers. |
| Secrets redaction (extended) | Each secret field is redacted on read and restored on save; non-secret fields pass through. |
| `SendTestMessage` (new) | Requires a session; audited on success and failure. |
| `TestAWSSMSLive` (new, opt-in) | Skipped unless `AWS_SMS_LIVE_TEST=1` and credentials are set. Sends from a simulator origination number to a simulator destination number. CI never runs it. |

**Manual:** configure both providers in the app against the AWS sandbox. Send a test SMS to
a simulator number and a test email to `success@simulator.amazonses.com`. Confirm the
password and secret key show as redacted after saving and reloading.

### PR 3: Automatic reminders

The largest PR. It contains the only schema changes.

**Schema** (edited declaratively, migration generated with `atlas migrate diff --env main`,
then `atlas migrate hash`). All changes are additive:

| Schema file | Change |
| --- | --- |
| `schema/notifications.sql` | `notification_log.reminder_kind TEXT` (nullable; NULL for manual sends). Partial unique index on `(appointment_id, reminder_kind, channel) WHERE reminder_kind IS NOT NULL`. |
| `schema/config.sql` | `practice_config.timezone TEXT DEFAULT ''`. New single-row `reminder_settings` table (enabled, quiet-hours start and end). New `reminder_rules` table (offset before the appointment, channel, provider, subject and body templates, enabled, timestamps). |

Existing installs get `timezone = ''`, and reminders stay off until someone sets a timezone.
That way no reminder is ever rendered in a guessed timezone.

**Modified**

| File | Change |
| --- | --- |
| `internal/domain/notification.go` | `NotificationStatusPending`; `ReminderKind` on `NotificationLog`. |
| `internal/domain/config.go` | `Timezone` on `PracticeConfig`. |
| `internal/storage/repository.go` | `NotificationLogRepository` gains `Claim` (insert a `pending` row; returns a conflict error if the reminder was already claimed), `UpdateResult`, and `CountReminderSends(patientID, channel, since)`. New `ReminderRepository` interface. |
| `internal/storage/sqlite/notification_repo.go` | Implement the new methods; read and write `reminder_kind`. |
| `internal/storage/sqlite/practice_config_repo.go` | Read and write `timezone`. |
| `internal/services/notification_service.go` | Move `SendNotification`'s body into an unexported, actor-aware `deliver`. Manual sends pass the session user; the scheduler passes `system:reminders`. Reminder sends claim the row before calling the provider. |
| `internal/services/config_service.go` | Validate the timezone with `time.LoadLocation`. |
| `main.go` | Import `time/tzdata` so timezones work on Windows, which has no system timezone database for Go. Build the reminder service and scheduler; register the service with Wails; start the scheduler in a goroutine and stop it when the app exits. |
| `frontend/src/views/ClinicView.svelte` | Add the Reminders section. |
| `frontend/src/views/clinic/ClinicProfileSection.svelte` | Timezone picker. |
| `frontend/src/components/PatientInfoPanel.svelte` | Notification history (uses the existing `ListNotificationLog`). |
| `frontend/src/components/AppointmentModal.svelte` | The appointment's reminder history (uses the existing `ListNotificationLogForAppointment`). |
| `frontend/messages/en.json` | Settings, history, statuses, and default templates. |
| `docs/lan-server-setup.md` | Reminders run on the server in LAN mode. |

**Added**

| File | Contents |
| --- | --- |
| `internal/storage/sqlite/migrations/<timestamp>_appointment_reminders.sql` | Generated by Atlas. |
| `internal/domain/reminder.go` | `ReminderRule`, `ReminderSettings`. |
| `internal/storage/sqlite/reminder_repo.go` + test | Reminder settings and rules storage. |
| `internal/services/reminder_service.go` + test | Wails-bound, token-gated get/save for settings and rules. Every change is audited under the staff member. |
| `internal/services/reminder_template.go` + test | Renders templates. Only the allowed placeholders exist (`{first_name}`, `{date}`, `{time}`, `{practice_name}`, `{practice_phone}`), so a template can't pull in chart data. Unknown placeholders are rejected when saved. |
| `internal/services/reminder_scheduler.go` + test | Not bound to Wails. A ticker loop with an injectable clock. Each pass loads settings and finds upcoming appointments. For each rule that's due it re-checks the appointment and patient, applies quiet hours and the TCPA limits, renders the message, and calls `deliver` as the system actor. |
| `frontend/src/views/clinic/RemindersSection.svelte` | On/off switch, quiet hours, rules, and template editor. Default templates come from Paraglide, so they're localized. Notes that desktop installs only send while the app is open. |

**Tests**

| Area | Checks |
| --- | --- |
| Migration upgrade (extend `TestMigrations_UpgradePreservesPatientData`) | Seed a `notification_log` row and a `practice_config` row on the previous schema, upgrade, and confirm both survive with `reminder_kind` NULL and `timezone` `''`. Roll back and confirm the down migration works. |
| Notification repo (extended) | `Claim` succeeds once and conflicts the second time; manual sends (NULL kind) never conflict; `UpdateResult`; `CountReminderSends` windows. |
| Reminder repo (new) | Settings and rules CRUD. |
| Practice config repo (extended) | `timezone` round-trips. |
| Template (new) | Every placeholder renders; unknown placeholders are rejected; date and time use the practice timezone and date format; SMS segment count is reported. |
| Scheduler (new), fake clock and dummy provider | Sends when due. Doesn't send twice across two passes. Skips cancelled, completed, and no-show appointments, rescheduled appointments, opted-out patients, and patients with no contact for the channel. Holds messages during quiet hours and sends them once quiet hours end. Enforces 1 per day and 3 per week across appointments. Renders correctly across a DST change. Catch-up at startup skips appointments that have already started. A provider failure is logged as `failed` and not retried. A row left `pending` (simulated crash) is not resent. Does nothing while disabled or without a timezone. |
| Scheduler concurrency (new) | Two schedulers on the same database run a pass at the same moment; exactly one message is sent. |
| Audit (new) | Every scheduler send, success or failure, writes an audit entry with `user_id = system:reminders`, the patient, and the `notification_log` ID. Settings changes are audited under the staff member. |
| Notification service (existing, kept passing) | `SendNotification` behaves as before after moving to `deliver`. |
| Reminder service (new) | Requires a session; validates rules; audits changes. |

**Manual:** with `task demo` data and the AWS sandbox, set the timezone, create a rule a few
minutes ahead of a demo appointment whose contact details are changed to your own number or a
simulator number, and watch it send once. Restart the app and confirm it doesn't resend.
Check the Audit view shows the system actor. Repeat in server mode (`task server`).

### PR 4: Optional Terraform module

Only after the maintainer agrees to adding Terraform to the repo.

**Added**

| File | Contents |
| --- | --- |
| `deploy/aws/notifications/versions.tf`, `variables.tf`, `main.tf`, `outputs.tf` | SES domain identity with DKIM and a configuration set; End User Messaging SMS configuration set; an IAM user whose policy only allows sending SMS from the practice's origination identity and sending email from its SES identity. No access key resource, so no secret ends up in Terraform state. Exact resource types will be checked against the current AWS provider. |
| `deploy/aws/notifications/README.md` | Prerequisites (BAA through AWS Artifact, 10DLC registration, leaving the sandboxes), `terraform apply`, creating the access key by hand, and entering values in the app. |
| `deploy/aws/notifications/tests/*.tftest.hcl` | `terraform test` with a mocked AWS provider: the IAM policy contains only send actions, and resources are scoped to the declared identities. |

**Modified**

| File | Change |
| --- | --- |
| `.github/workflows/ci.yml` | New job: `terraform fmt -check`, `init -backend=false`, `validate`, `tflint`, `terraform test`. No AWS credentials in CI. |
| `Taskfile.yml` | `task iac:check` runs the same checks locally. |
| `.gitignore` | `.terraform/`, `*.tfstate*`. |
| `docs/notification-alerts.md` | Link to the module. |

**Manual:** `terraform apply` and `terraform destroy` against the developer sandbox account
(not root credentials).

### What is not covered by automated tests

- **Frontend behavior.** The repo has no frontend test framework. Svelte changes are checked
  by `svelte-check` (types) and by hand. Adding one is out of scope.
- **Real carrier and inbox delivery.** CI only uses fakes and recorded responses. Real AWS
  calls are opt-in, and only reach simulator numbers and SES's mailbox simulator.
- **Vendor API drift.** The recorded AWS responses can go stale, as with Stedi. The opt-in
  live test is the check for that.

## Developer testing with AWS

Use a personal AWS account for development, with these rules:

- **Never put real patient data in it.** Use `task demo` data, your own phone and email, and
  AWS's simulator numbers and addresses. A personal account has no BAA.
- **Stay in the sandboxes.** New accounts start with SMS in sandbox (verified destinations
  only) and SES in sandbox (verified addresses only, 200 emails per day).
- **Use the SMS simulator** for most testing. Simulator origination numbers (US only) can only
  text simulator destination numbers. Messages never reach a carrier, but realistic delivery
  events still come back, and no 10DLC registration is needed. Verify your own phone only to
  check how a real message looks.
- **Use the SES mailbox simulator** (`success@simulator.amazonses.com`,
  `bounce@simulator.amazonses.com`, and so on) to test bounces and complaints.
- **Keep the spend limit.** New accounts have a $1/month SMS limit. Leave it, and add an AWS
  Budgets alert as a backstop.
- **Separate credentials.** Your CLI login is for you. The app gets its own IAM user that can
  only send SMS and email, and its key goes into the app's settings, never into `~/.aws`.
  Don't use root credentials. Don't let Terraform create access keys, because the secret
  would be stored in the state file.
- **Live tests are opt-in.** Following the Stedi pattern, tests that call AWS only run when
  an environment variable is set, and CI never has AWS credentials.

## Open questions

- Which reminder rules ship by default (for example 48 hours by SMS and email, 2 hours by
  SMS)?
- Should templates be per-language, using the patient's `preferred_language`?
- Should reminders respect `preferred_contact_method`, or send on every channel the practice
  enables?
- Two-way replies ("reply C to confirm") through SNS to SQS polling: in scope for a later
  phase?

## References

- [HHS: Does HIPAA permit providers to use e-mail with patients?](https://www.hhs.gov/hipaa/for-professionals/faq/570/does-hipaa-permit-health-care-providers-to-use-email-to-discuss-health-issues-with-patients/index.html)
- [HIPAA Security Rule technical safeguards, 45 CFR 164.312](https://www.ecfr.gov/current/title-45/subtitle-A/subchapter-C/part-164/subpart-C/section-164.312)
- [NIST SP 800-53 AU-3: Content of Audit Records](https://www.upguard.com/compliance/nist-sp-800-53/au/au-3)
- [HL7 FHIR R4 AuditEvent definitions](https://hl7.org/fhir/R4/auditevent-definitions.html)
- [K&L Gates: FCC clarifies TCPA rules for health care communications (2015)](https://klgates.com/Health-Care-Entities-Get-Clarity-from-FCC-on-Telephone-Communications-08-07-2015)
- [Feldesman: Sending patient appointment reminders? Don't forget the FCC](https://www.feldesman.com/compliance-corner-sending-patient-appointment-reminders-dont-forget-the-fcc/)
- [Transactional outbox with at-least-once delivery](https://oneuptime.com/blog/post/2026-07-22-transactional-outbox-duplicate-events/markdown)
- [AWS HIPAA eligible services](https://aws.amazon.com/compliance/hipaa-eligible-services-reference/)
- [Twilio HIPAA accounts](https://www.twilio.com/docs/iam/twilio-editions/hippa)
- [AWS End User Messaging SMS: simulator phone numbers](https://docs.aws.amazon.com/sms-voice/latest/userguide/test-phone-numbers.html)
- [AWS End User Messaging SMS: quotas](https://docs.aws.amazon.com/sms-voice/latest/userguide/quotas.html)
- [AWS End User Messaging SMS: spend limits](https://docs.aws.amazon.com/sms-voice/latest/userguide/spend-limit.md)
