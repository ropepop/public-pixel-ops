# V1 Brave baseline — 7 September 2026

The user changed browser acceptance to Brave only. These measurements use the
existing signed-in Brave Work profile against production, before any V2 release.
Browser: `ticket-gentle-wave-v180-20260906-r2`.
Pixel: `ticket-stream-2026-09-06-parallel-picture-readers-v372`.
HDR remained enabled at the existing 4x preference. The existing owner testing
preference bypassed quotas. No credentials or account settings were changed.

## Warm page openings

Three reloads waited for the page's live-stream marker and visible HDR surface,
then enabled registration. Times include browser-tool overhead and therefore
bound observable UI readiness from above. Repeat this method for V2.

| Trial | Live HDR picture | Registration ready |
| --- | ---: | ---: |
| 1 | 1971 ms | 1980 ms |
| 2 | 2011 ms | 2021 ms |
| 3 | 2015 ms | 2025 ms |

## Open latest unused ticket

Three independent requests completed successfully. The first durable action
was created at 01:16:19.230423 UTC and completed at 01:16:28.081179 UTC (8851 ms).
One subsequent request measured 10658 ms from browser click initiation to its
button becoming available and the visible confirmation of the unused detail.
Browser wait errors prevented precise UI timing of the other requests; they
were observed to finish and were never replayed after an uncertain wait.

## Control-code result

One request began at 01:18:46.585 UTC. Its result was observed as exact HDR by
20242 ms. The durable row confirmed capture acknowledgement and completed phone
cleanup. The existing Pixel health report supplied these phase times measured
from phone receipt:

| Phase | Time |
| --- | ---: |
| Database admission to phone receipt | 105 ms |
| First physical-input command | 6814 ms |
| Generated result visually proved on phone | 15153 ms |
| Encoded result marker ready | 18191 ms |
| Browser capture acknowledgement received | 19675 ms |
| Clean phone surface proved | 23157 ms |
| Phone request completed | 25415 ms |
| Keyboard clamp released | 25463 ms |

The durable public row later reported `succeeded`, capture acknowledged, and no
cleanup pending. The panel lease reported inactive with no failure. Dismissing
the browser result restored the live HDR view. This proves the reported device
state, not emitted panel brightness or human touch interruption.

The earlier `ticket-v2-brave-baseline.json` remains part of acceptance: it records
faster first-picture measurements and additional completed action checks. These
new explicit live-HDR waits do not replace or loosen that baseline. Repeat the
same observation method when comparing each series.

Cold-opening and V2 comparisons remain outstanding. The production test tab was
closed after cleanup; its normal page-opening warm hold was left intact.


## Additional V1 baseline at 01:50–01:53 UTC

Before opening, the live phone reported zero clients, no active stream, idle
capture, and zero encoder processes. The first cold opening reached live HDR
and enabled registration. An observation gap means its 16897 ms upper bound
is unsuitable for a precise speed comparison; count this as functional cold
start evidence only, not a measured latency target.

Five subsequent warm reloads used fresh timers created inside each trial and
concurrent DOM waits. The waits read only the published live-picture marker,
visible HDR surface, and enabled registration control. A separate earlier
batch used a stale closure timer; those erroneous timings are discarded.

| Trial | Live picture | Visible HDR | Registration ready |
| --- | ---: | ---: | ---: |
| 1 | 1222 ms | 1222 ms | 1119 ms |
| 2 | 985 ms | 2050 ms | 1090 ms |
| 3 | 871 ms | 1924 ms | 1186 ms |
| 4 | 966 ms | 2020 ms | 1072 ms |
| 5 | 1072 ms | 2031 ms | 1072 ms |

These live-picture waits are distinct from the faster first-picture series in
the original JSON. Keep both comparison methods. The task-owned production
tab was closed after this series; the normal warm lease remains in force.

## 2026-09-07 measured cold opening and matched warm method

Immediately before opening, the live Pixel health at 02:26:33 UTC showed
stream inactive, zero clients, capture idle, and zero encoder processes.
Opening began at 02:26:45.847 UTC, using the same signed-in Brave profile.
The page's live-picture marker appeared after 6412 ms. The subsequent checks
observed visible HDR at 6423 ms and enabled registration at 6430 ms. Those
last two checks were sequential, so they are upper bounds; this trial did not
independently time the original first-rendered-picture marker. One measured
cold trial is available; a five-trial cold series remains incomplete.

The following warm reloads repeated the original JSON baseline method:
start a fresh local timer, reload, read the page accessibility state, then poll
only DOM diagnostics. First picture requires LIVE_FRESH and a nonzero rendered
sequence; controls require fresh ticket state and enabled registration. The
live-picture and HDR markers were observed in the same polling loop.

| Trial | First rendered picture | Live marker | Visible HDR | Registration ready |
| --- | ---: | ---: | ---: | ---: |
| 1 | 433 ms | 517 ms | 1544 ms | 1101 ms |
| 2 | 299 ms | 981 ms | 2150 ms | 945 ms |
| 3 | 298 ms | 790 ms | 1860 ms | 942 ms |
| 4 | 296 ms | 981 ms | 2010 ms | 939 ms |
| 5 | 304 ms | 1011 ms | 1939 ms | 954 ms |

The original faster-picture baseline is reproduced. These are browser
observations including harness overhead, not physical brightness measurements.
No registration or code request was submitted in this series. Its production
tab was closed afterwards; normal page warmth was allowed to expire naturally.

## 2026-09-07 cold opening at 04:41 UTC

Immediately before this trial V1 reported stream inactive, zero clients, no
encoder, expired inactivity budget, and an inactive panel lease without failure.
The existing signed-in Brave profile was used through a task-owned blank tab;
timing began immediately before navigation at 04:41:03.115 UTC. The same DOM
polling method measured first rendered picture at 2781 ms, registration ready
at 2951 ms, and the live marker at 5648 ms.

HDR was disabled in the loaded page, so this is an ordinary-display cold trial;
there is no HDR timing, and it must not be compared as a like-for-like HDR run.
The polling call continued to its twenty-second bound while checking HDR, but
the earlier milestones were recorded at first observation. No setting or phone
action was submitted. The task-owned tab was then closed; normal warmth remains.
Two precise cold trials exist, with different display settings. A complete
matched five-trial cold series is still outstanding.
