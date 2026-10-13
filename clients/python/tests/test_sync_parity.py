"""phonetopc.sync must mirror the async API: every public member, same parameters.

The sync wrappers are written by hand, so they drift when the async classes
grow. These tests introspect live objects (instance attributes only exist on
instances) and fail when a public member of :class:`~phonetopc.Phone`,
:class:`~phonetopc.Subscription` or :class:`~phonetopc.Panel`, or a parameter
of :func:`phonetopc.connect`, has no counterpart in :mod:`phonetopc.sync`.

Intended differences go in the allowlists below, each with a reason.
"""

from __future__ import annotations

import inspect
from collections.abc import Iterator
from typing import Any

import pytest
from fakeserver import FakeServer

import phonetopc
import phonetopc.sync
from phonetopc import ui

#: async dunder -> the sync dunder that replaces it.
DUNDERS = {
    "__aenter__": "__enter__",
    "__aexit__": "__exit__",
    "__aiter__": "__iter__",
    "__anext__": "__next__",
}

#: Public async members with no sync counterpart, by async class name.
ASYNC_ONLY: dict[str, set[str]] = {
    "Phone": set(),
    "Subscription": set(),
    "Panel": set(),
}

#: Public sync members with no async counterpart, by sync class name.
SYNC_ONLY: dict[str, set[str]] = {
    # The wrapped async object, for code that mixes both APIs.
    "SyncPhone": {"phone"},
    "SyncSubscription": {"subscription"},
    "SyncPanel": {"panel"},
}

#: Members that may be assigned on the sync wrapper (they configure later calls).
#: Everything else is a read-only view of the async object's state.
SETTABLE: dict[str, set[str]] = {
    "SyncPhone": {"request_timeout", "queue_size"},
    "SyncSubscription": set(),
    "SyncPanel": set(),
}

#: Parameters of phonetopc.connect that phonetopc.sync.connect leaves out (none).
CONNECT_ASYNC_ONLY: set[str] = set()


@pytest.fixture
def pairs(threaded_server: FakeServer) -> Iterator[list[tuple[Any, Any]]]:
    """(async object, sync wrapper) for each wrapped class, all live."""
    with phonetopc.sync.connect(threaded_server.addr, dev=True) as phone:
        panel = phone.show_panel("p", "P", ui.button("b", "B"))
        with phone.subscribe("battery") as sub:
            yield [
                (phone.phone, phone),
                (sub.subscription, sub),
                (panel.panel, panel),
            ]


def public(obj: Any) -> set[str]:
    return {n for n in dir(obj) if not n.startswith("_")}


def params(fn: Any) -> list[tuple[str, Any, Any]]:
    """Name, kind and default of each parameter; annotations differ by design."""
    sig = inspect.signature(fn)
    return [(p.name, p.kind, p.default) for p in sig.parameters.values() if p.name != "self"]


def test_members_match(pairs: list[tuple[Any, Any]]) -> None:
    for aobj, sobj in pairs:
        aname, sname = type(aobj).__name__, type(sobj).__name__
        missing = public(aobj) - ASYNC_ONLY[aname] - public(sobj)
        assert not missing, f"{sname} lacks {sorted(missing)} of {aname}"
        extra = public(sobj) - SYNC_ONLY[sname] - public(aobj)
        assert not extra, f"{sname} has {sorted(extra)} that {aname} doesn't"
        for adunder, sdunder in DUNDERS.items():
            if hasattr(type(aobj), adunder):
                assert hasattr(type(sobj), sdunder), f"{sname} lacks {sdunder}"


def test_member_kinds_and_signatures(pairs: list[tuple[Any, Any]]) -> None:
    for aobj, sobj in pairs:
        acls, scls = type(aobj), type(sobj)
        for name in sorted(public(aobj) - ASYNC_ONLY[acls.__name__]):
            where = f"{scls.__name__}.{name}"
            amember = inspect.getattr_static(acls, name, None)
            smember = inspect.getattr_static(scls, name)
            if amember is None or isinstance(amember, property):
                # Instance attribute or property: a property on the sync side.
                assert isinstance(smember, property), f"{where} should be a property"
                settable = smember.fset is not None
                expected = name in SETTABLE[scls.__name__]
                assert settable == expected, f"{where}: settable={settable}, expected {expected}"
                assert getattr(sobj, name) == getattr(aobj, name), f"{where} differs"
                continue
            assert callable(smember), f"{where} should be a method"
            assert not inspect.iscoroutinefunction(smember), f"{where} must block, not be async"
            assert params(smember) == params(amember), f"{where} parameters differ"


def test_connect_signature() -> None:
    aparams = [p for p in params(phonetopc.connect) if p[0] not in CONNECT_ASYNC_ONLY]
    assert params(phonetopc.sync.connect) == aparams


def test_wrapped_coroutines_block() -> None:
    """No public coroutine function leaks into phonetopc.sync."""
    for cls in (
        phonetopc.sync.SyncPhone,
        phonetopc.sync.SyncSubscription,
        phonetopc.sync.SyncPanel,
    ):
        for name, member in inspect.getmembers(cls):
            assert not inspect.iscoroutinefunction(member), f"{cls.__name__}.{name} is async"
