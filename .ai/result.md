# Ticket V2 release status

V2 is deployed and the requested live acceptance is complete, with the explicit
exclusions and measurement limits below.
The user accepted the measured 40.17% production-code reduction and waived the
original 50% target. Testing quota remains unlimited as requested.

## Deployed identity

- Ops V2: 72af3e29, followed by deployment-check correction ff6326cf,
  browser action-source correction 7ecfa21a, warm-capture correction a3f9c272,
  and multi-pointer cancellation 3901acd4.
- Production server release: ticket-v2-v181-20260907-r5; full deployment and
  validation passed at 11:46 UTC on 7 September.
- Pixel V2: 4b81977; standard ticket_screen deployment passed in run
  20260907T110806Z-9287. Installed APK SHA256:
  3b6adaf7ecc784b47f86963376e3ecb2a6cc79f1cd9027fe2b00e7c2b8555501.
- Existing database ticket-remote-prod-v3 was updated with delete-data=never.
  Its identity and recorded outcomes were retained. Maintenance is disabled.

The additive V1 maintenance gate was published first, admissions were paused,
then pending commands, running actions, queued intents, code cleanup and the
phone journal were proved drained. V2 publication preserved the maintenance
state. Pixel and server deployment followed. The first server release failed
an obsolete final static check and automatically rolled back; no acceptance
action had been submitted. The check was corrected before continuing.

## What changed

Browser connection, current request and picture presentation each have one owner.
Phone recognition remains authoritative for ticket identity and input geometry.
Durable action completion has one atomic finalizer and a retained fingerprint.
Control-code delivery is tied to the exact encoded picture and its acknowledgement;
delivery recovery does not replay the physical action. Phone input, display and
physical-touch guards remain in place. Retired execution paths, duplicate
recognition, overlapping recovery and unused tracing were removed.

During live acceptance, unsupported browser action-source labels were corrected,
warm viewers were allowed to request a fresh capture without a cached picture,
and a second pointer now cancels slider authorization. Every deployed correction
reset all acceptance streaks. Earlier trial records are historical only.

## Verified

Local release checks passed: full Ticket tests, generated client checks, browser
build and TypeScript checks, focused Go race checks, and Android debug/release
tests, assemblies and release lint under Java 17. The latest Android test total
was 447 per variant. The initial Java 25 lint failure was resolved by using the
installed Java 17 runtime without a source workaround. The warm-capture correction
has a regression test proving new capture credit without a cached picture.

The current r5 acceptance ledger is `.ai/ticket-v2-acceptance.json` and is the
source of truth for completed trials. All five routes reached five successful
results: register-current button, open-newest-and-register button, real slider,
code button and top-left code shortcut. The final shortcut result expired
normally after its exact HDR image was inspected; a supplementary successful
request verified manual dismissal and live return. That supplementary capture
was black while Brave was in the background, so it is not counted as a second
visual proof. Earlier trials affected by Mac locking or concurrent browser use
are separately marked as interrupted observations, not product failures.

Each completed five-trial route includes ordinary display, reload, brief network
interruption with recovery, and two HDR trials. Each action matched a succeeded
database record, phone completion with an inactive panel lease and zero failures,
and the expected browser result. Slider trials used real input gestures.
Negative checks rejected reverse, short, vertical, canceled, multi-pointer,
connection-invalidated and phone-context-invalidated gestures without registration.
The shortcut boundary and inert behavior during a dialog/result were checked.

HDR code preflight displayed the exact requested 1234 result, acknowledged its
picture, cleaned up phone code/keyboard state, and restored live HDR on dismissal.
Admin checks covered unlimited preference, statistics layouts, create/cancel of
a future schedule, force redetection, and non-destructive saved-account sign-in.
Both directions of the ticket-view switch succeeded. Full reset was not exercised
against the linked live account.

## Timing and final state

The user excluded brightness verification and the real active-action touch
interruption check on 7 September. Neither is claimed as verified.

Ten matched warm reloads completed. Median first picture, live marker and visible
HDR were each 310.5 ms; median enabled registration was 1,037 ms. The matched V1
series had medians of 299 ms, 981 ms, 1,939 ms and 945 ms respectively. Thus HDR and
the live marker improved substantially, first picture was close, and registration
readiness measured 92 ms slower. These non-simultaneous measurements include
network and harness overhead; they do not establish strict no-regression on every
metric. The initial V2 series and the follow-up series are both retained in
`.ai/ticket-v2-brave-results.json`; no unfavorable sample was discarded.

A true cold start at 14:07 UTC produced the first fresh live picture and enabled
controls in 2,969 ms, with HDR visible in 2,975 ms. The V1 HDR cold reference was
6,412 ms for live picture, 6,423 ms for HDR and 6,430 ms for controls. This is one
cold comparison, not a latency distribution.

Natural shutdown was verified at 14:06:52 UTC before the cold test: streaming off,
zero video clients, capture idle, zero encoder processes, and inactive action
panel with zero failures. Other legitimate page openings had extended the wait;
no forced shutdown was used. The final cold-test tab is closed. That passive
opening begins a new normal warm lease, so encoder shutdown is not claimed again
after it.
At the latest durable check, pending commands, running actions, queued intents
and pending code cleanup were all zero, maintenance was off, and all four Ticket
containers were running (all three containers with health checks healthy).
Mirror audit and normal-user artifact permissions were clean.

## Size and evidence limits

Audited starting production scope: 91,205 physical / 86,456 nonblank lines.
Accepted candidate: 54,571 physical / 51,227 nonblank lines, or 40.17% / 40.75%
reduction. Eleven dedicated helper/assets files omitted from the original count
were restored to the audited scope using their original revisions. Test deletion,
generated output and shared-code relocation do not count. A fresh count after maintenance and release corrections is 54,630 physical /
51,283 nonblank lines: 40.10% / 40.68% reduction. The counter still returns failure
against the original frozen 50% ceiling; that ceiling was waived by the user,
not changed in the counter. There is no positive shared-code growth.

The existing signed-in Brave profile is preserved. The bundled dedicated Chrome
transport was absent from the live inventory; authorized CUA control of Brave
provided browser verification. Browser evidence does not prove physical HDR
brightness. Earlier V1 timings and local fixtures are not V2 production evidence.
