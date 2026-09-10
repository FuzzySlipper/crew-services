# Xbox mapping trace — #7905

## Finding and correction

The Wolf target accepted the conventional request names `a`, `b`, `x`, `y`,
`lb`, `rb`, `lt`, and `rt`, but its old packet map associated `x` with `0x4000`
and `y` with `0x8000`.  On this Wolf virtual device, those packet bits drive
the opposite physical face positions: `0x4000` reaches `BTN_NORTH` (Xbox Y)
and `0x8000` reaches `BTN_WEST` (Xbox X).  Firefox identified the device as a
standard-mapped gamepad, so the old target made a requested X visible at index
3 and a requested Y visible at index 2.

`internal/playtest/target/target.go` now translates the Wolf-device layout at
the target boundary: `x` is `0x8000` and `y` is `0x4000`.  No Rusty Engine or
product binding was changed.  The correction is deliberately scoped to this
Wolf virtual-device wire-bit-to-evdev layout; it does not redefine generic
XInput constants.

## End-to-end receipt

The target emitted a Moonlight controller packet, Wolf exposed it as its
virtual Xbox device, and a temporary page in the actual remote Firefox session
called `navigator.getGamepads()` every 50 ms after native activity.  The page
POSTed its own observed JSON to the local diagnostic server; it did not use an
Engine ingress or product binding as a substitute for browser metadata.

Firefox reported this identity on every active row:

```json
{"id":"045e-02ea-Wolf X-Box One (virtual) pad","mapping":"standard","index":0,"connected":true}
```

| Requested control | Corrected Wolf packet bits | Observed wire-bit mapping (pre-fix trace) | Firefox active `buttons` index |
| --- | ---: | --- | ---: |
| A | `0x1000` | `EV_KEY BTN_SOUTH (304)` | 0 |
| B | `0x2000` | `EV_KEY BTN_EAST (305)` | 1 |
| X | `0x8000` | `EV_KEY BTN_WEST (308)` | 2 |
| Y | `0x4000` | `EV_KEY BTN_NORTH (307)` | 3 |
| LB | `0x0100` | `EV_KEY BTN_TL (310)` | 4 |
| RB | `0x0200` | `EV_KEY BTN_TR (311)` | 5 |
| LT | byte 10 = `255` | `EV_ABS ABS_Z (2) = 255` | 6 |
| RT | byte 11 = `255` | `EV_ABS ABS_RZ (5) = 255` | 7 |

The pre-fix Firefox active rows were `A=0, B=1, X=3, Y=2, LB=4, RB=5, LT=6,
RT=7`.  After deployment, the exact active rows, in request order, were:

```json
[{"buttons":[0]},{"buttons":[1]},{"buttons":[2]},{"buttons":[3]},
 {"buttons":[4]},{"buttons":[5]},{"buttons":[6]},{"buttons":[7]}]
```

Each row retained the identity and `mapping:"standard"` shown above. The full
diagnostic POST bodies (including neutral rows and their
capture times) are retained in
[`xbox-mapping-7905-navigator-posts.json`](xbox-mapping-7905-navigator-posts.json).
The corrected action receipt was `d9d17a3e-9c2e-44f5-9826-797dc93966bf`; it
completed all 15 requested input/wait steps with `release_errors: []`.

## Raw native receipt

The table's evdev column is the observed **pre-fix wire-bit mapping**. Before
the correction, the `Wolf X-Box One (virtual) pad` (`045e:02ea`, `event16`)
was read during a native A/B/X/Y/LB/RB/LT/RT packet batch. The
following is the retained base64 of its 32 little-endian 24-byte Linux
`input_event` records (press/release pairs).  It is intentionally kept as the
raw receipt behind the decoded evdev column above.

```text
Gc+gagAAAABybQQAAAAAAAEAMAEBAAAAGc+gagAAAABybQQAAAAAAAAAAAAAAAAAGc+gagAAAABWQwgAAAAAAAEAMAEAAAAAGc+gagAAAABWQwgAAAAAAAAAAAAAAAAAGc+gagAAAABLTwgAAAAAAAEAMQEBAAAAGc+gagAAAABLTwgAAAAAAAAAAAAAAAAAGc+gagAAAABOJAwAAAAAAAEAMQEAAAAAGc+gagAAAABOJAwAAAAAAAAAAAAAAAAAGc+gagAAAAD/MAwAAAAAAAEAMwEBAAAAGc+gagAAAAD/MAwAAAAAAAAAAAAAAAAAGs+gagAAAACtwgAAAAAAAAEAMwEAAAAAGs+gagAAAACtwgAAAAAAAAAAAAAAAAAAGs+gagAAAAABzwAAAAAAAAEANAEBAAAAGs+gagAAAAABzwAAAAAAAAAAAAAAAAAAGs+gagAAAAA7owQAAAAAAAEANAEAAAAAGs+gagAAAAA7owQAAAAAAAAAAAAAAAAAGs+gagAAAAAQsAQAAAAAAAEANgEBAAAAGs+gagAAAAAQsAQAAAAAAAAAAAAAAAAAGs+gagAAAADSgggAAAAAAAEANgEAAAAAGs+gagAAAADSgggAAAAAAAAAAAAAAAAAGs+gagAAAACVjggAAAAAAAEANwEBAAAAGs+gagAAAACVjggAAAAAAAAAAAAAAAAAGs+gagAAAABQYgwAAAAAAAEANwEAAAAAGs+gagAAAABQYgwAAAAAAAAAAAAAAAAAGs+gagAAAABFbgwAAAAAAAMAAgD/AAAAGs+gagAAAABFbgwAAAAAAAAAAAAAAAAAG8+gagAAAACgAAEAAAAAAAMAAgAAAAAAG8+gagAAAACgAAEAAAAAAAAAAAAAAAAAG8+gagAAAAD5EQEAAAAAAAMABQD/AAAAG8+gagAAAAD5EQEAAAAAAAAAAAAAAAAAG8+gagAAAAD05QQAAAAAAAMABQAAAAAAG8+gagAAAAD05QQAAAAAAAAAAAAAAAAA
```

Relevant browser captures are retained at:

- `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/dcfda30c-85cb-477d-8b71-ed92833116fd/a72bb661-b5e3-42fa-91a1-0b657f706e49.png` (pre-fix diagnostic session)
- `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/fb12457e-15d6-43ed-a57a-6cf89139fc8e/b611cb71-2784-4c1a-a2e8-8f2377544d82.png` (corrected diagnostic session launch)

Both disposable sessions were explicitly released: pre-fix
`dcfda30c-85cb-477d-8b71-ed92833116fd` at `2026-09-09T03:22:45Z`, corrected
`fb12457e-15d6-43ed-a57a-6cf89139fc8e` at `2026-09-09T03:24:22Z`; both
reported `errors: []` and the target lease was then null.

## Verification and deployment

`go test ./internal/playtest/target` passed after adding a packet-level test
that checks the individual A/B/X/Y/LB/RB bits and LT/RT bytes.  The deployed
`playtest-target` SHA-256 was
`f849ebe0dff6d977828f4b3ef498bcdc2177173b0fcf4758242a4f32ef4aad3d`; the
remote `den-playtest-controller.service` was active before the corrected
Firefox replay.  The temporary `xbox-mapping-7905` profile was removed from
the installed games file and `crew-playtest.service` was restarted afterward,
leaving no live playtest session.
