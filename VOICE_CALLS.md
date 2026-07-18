# Experimental voice call bridge

This fork can bridge one-to-one voice calls between WhatsApp and Matrix. It uses
meowcaller for WhatsApp signaling/media and Pion WebRTC for Matrix. Audio is
transcoded between WhatsApp's 16 kHz mono PCM frames and Matrix Opus at 48 kHz.

Enable `voice_calls` in the bridge configuration and configure a fixed UDP port
range. The same range must be reachable from Matrix clients. In a container,
either use host networking or publish the full UDP range and set `public_ip` to
the address used by that port mapping.

For clients behind NAT, configure `turn_uris` and set `turn_shared_secret` to the
same secret as coturn's `static-auth-secret`. The bridge creates short-lived TURN
REST credentials in memory; never commit that secret to this repository.

Only one-to-one audio calls are supported. Group calls, video, hold, and call
transfer are deliberately rejected. `max_concurrent_per_login` bounds resource
use, while `ring_timeout` and `connect_timeout` clean up abandoned sessions.

The `Voice call image` GitHub Actions workflow tests and publishes the custom
image. On its daily schedule it first merges the current upstream `main`. A merge
conflict stops the workflow and leaves the last working container image intact,
so upstream changes cannot silently discard the call implementation.
