"""crew: the Hermes side of crew-hermes, the crew messaging driver.

crew-hermes delivers ordinary crew mail into this profile's most recent chat
through the gateway. Mail its sender marked no_wake must never start a turn,
so crew-hermes leaves it in ~/.crew-hermes/pending/<profile>/ and this plugin
attaches it to the next turn the profile runs anyway, through pre_llm_call.
A file is moved to taken/ before its text is used, so it is attached once.

The first turn of each chat also gets the profile's crew address, seats and
the crew-hermes board command, which crew-hermes keeps in
~/.crew-hermes/profiles/<profile>.json.
"""
import json
import os
from pathlib import Path

STATE = Path(os.environ.get("CREW_HERMES_STATE") or Path.home() / ".crew-hermes")


def _installed_profile():
    """The profile this copy was installed in: .../profiles/<name>/plugins/crew,
    or the default profile's .../.hermes/plugins/crew."""
    here = Path(__file__).resolve().parent
    if here.parent.parent.parent.name == "profiles":
        return here.parent.parent.name
    return "default"


INSTALLED = _installed_profile()


def _profile():
    try:
        from gateway.session_context import get_session_env

        return get_session_env("HERMES_SESSION_PROFILE") or INSTALLED
    except Exception:
        return INSTALLED


def _intro(profile):
    try:
        about = json.loads((STATE / "profiles" / (profile + ".json")).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return ""
    return str(about.get("context") or "")


def _pending(profile):
    pending = STATE / "pending" / profile
    taken = STATE / "taken" / profile
    texts = []
    try:
        names = sorted(p.name for p in pending.iterdir() if p.suffix == ".json")
    except OSError:
        return texts
    taken.mkdir(parents=True, exist_ok=True)
    for name in names:
        try:
            os.rename(pending / name, taken / name)  # claims it: a second caller finds it gone
        except OSError:
            continue
        try:
            texts.append(str(json.loads((taken / name).read_text(encoding="utf-8"))["text"]))
        except (OSError, ValueError, KeyError):
            continue
    return texts


def register(ctx):
    introduced = set()

    def pre_llm_call(session_id=None, is_first_turn=False, **kwargs):
        profile = _profile()
        parts = []
        if session_id and session_id not in introduced:
            introduced.add(session_id)
            intro = _intro(profile)
            if intro:
                parts.append(intro)
        parts.extend(_pending(profile))
        if not parts:
            return None
        return {"context": "\n\n".join(parts)}

    ctx.register_hook("pre_llm_call", pre_llm_call)
