"""crew-hermes delivery helper: puts one crew message into a Hermes profile's
most recent chat that takes messages through the gateway's own client, the way `hermes chat` and
the Desktop forwarder do. crew-hermes runs it with Hermes's bundled Python,
HERMES_HOME set to the profile's home and CREW_HERMES_AGENT to the
hermes-agent checkout. It reads one JSON request on stdin and writes one JSON
result on stdout:

  request: {"profile", "mode": "queue"|"steer", "text", "request_id"}
  result:  {"ok": true, "session", "via": "steer"|"submit", "status", "admission_id"}
           {"ok": false, "definite": bool, "code", "error"}

"definite" means the gateway refused before admitting anything, so the
message is certainly not in the chat.
"""
import asyncio
import json
import os
import sys

sys.path.insert(0, os.environ["CREW_HERMES_AGENT"])
try:
    import hermes_bootstrap  # noqa: F401  (Hermes's launchers import it first)
except ImportError as exc:
    if exc.name != "hermes_bootstrap":
        raise


# Chats that never take a message from outside: scheduled jobs answer only
# their job, and delegated, kanban and one-shot runs are not conversations.
DENIED_SOURCES = {"cron", "subagent", "kanban", "oneshot", "unknown", "api_server", "webhook"}
CREW_CHAT = "Crew"


def recent_chats(limit=5):
    """The profile's chats that can take a message, most recently active
    first, read from its own state.db."""
    import sqlite3

    path = os.path.join(os.environ["HERMES_HOME"], "state.db")
    db = sqlite3.connect("file:" + path + "?mode=ro", uri=True)
    try:
        rows = db.execute("SELECT id, source FROM sessions WHERE COALESCE(hidden, 0) = 0 AND COALESCE(archived, 0) = 0"
                          " AND COALESCE(auto_archived, 0) = 0 ORDER BY last_activity_at DESC LIMIT 200").fetchall()
    finally:
        db.close()
    return [sid for sid, source in rows if (source or "unknown") not in DENIED_SOURCES][:limit]


class Stage:
    """How far a delivery got: an error before the submit is sent means
    nothing reached the chat."""
    submitting = False


async def deliver(request, stage):
    from hermes_cli.gateway_client import GatewayRPCError, connect_gateway

    async with connect_gateway() as client:
        stored = target = None
        for candidate in recent_chats():
            try:
                attached = await client.rpc("session.resume", session_id=candidate, profile=request["profile"])
            except GatewayRPCError:
                continue  # a chat this client cannot open (another surface's); try the next
            stored, target = candidate, str(attached.get("session_id") or candidate)
            break
        if target is None:
            # The profile has no chat that takes messages: start one for crew mail.
            created = await client.rpc("session.create", profile=request["profile"], title=CREW_CHAT)
            target = stored = str(created.get("session_id") or created.get("id") or "")
            if not target:
                return {"ok": False, "definite": True, "code": "no-chat", "error": "the gateway created no chat"}
        if request["mode"] == "steer":
            stage.submitting = True
            try:
                steered = await client.rpc("session.steer", session_id=target, text=request["text"])
            except GatewayRPCError:
                steered = {}
            stage.submitting = False
            if steered.get("status") == "queued":
                return {"ok": True, "session": stored, "via": "steer", "status": "steered"}
            # No turn is running to steer into: it becomes the next turn.
        stage.submitting = True
        receipt = await client.rpc("prompt.submit", session_id=target, text=request["text"], queued=True,
                                   submission_id=request["request_id"])
        return {"ok": True, "session": stored, "via": "submit", "status": receipt.get("status"),
                "admission_id": receipt.get("admission_id")}

def main():
    request = json.load(sys.stdin)
    stage = Stage()
    try:
        result = asyncio.run(deliver(request, stage))
    except Exception as exc:  # unknown, unless nothing was sent or the gateway refused before its write
        from hermes_cli.gateway_client import GatewayRPCError
        try:
            from tui_gateway.owner_forward import _REFUSED_BEFORE_COMMIT as refused
        except ImportError:
            refused = ()
        reason = (exc.data or {}).get("reason") if isinstance(exc, GatewayRPCError) else None
        definite = not stage.submitting or (isinstance(reason, str) and reason in refused)
        result = {"ok": False, "definite": definite, "code": reason or type(exc).__name__, "error": str(exc)[:500]}
    json.dump(result, sys.stdout)


if __name__ == "__main__":
    main()
