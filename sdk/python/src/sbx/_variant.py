"""A method that can be called on the class or on an instance, with different signatures.

``Sandbox.kill("sb-...")`` kills a sandbox by id; ``sandbox.kill()`` kills
that sandbox. E2B's SDKs offer both under one name, so this one does too.
"""

from __future__ import annotations

from typing import Any, Generic, TypeVar, overload

C = TypeVar("C")
I = TypeVar("I")  # noqa: E741


class ClassOrInstanceMethod(Generic[C, I]):
    """Dispatch an attribute to one method on the class and another on instances.

    ``C`` and ``I`` are the call signatures (as callback protocols) seen by
    a type checker for ``Class.name`` and ``instance.name``.
    """

    def __init__(self, class_method: str, instance_method: str, doc: str = "") -> None:
        self._class_method = class_method
        self._instance_method = instance_method
        self.__doc__ = doc

    @overload
    def __get__(self, obj: None, owner: type[Any]) -> C: ...
    @overload
    def __get__(self, obj: object, owner: type[Any] | None = None) -> I: ...

    def __get__(self, obj: object | None, owner: type[Any] | None = None) -> C | I:
        if obj is None:
            return getattr(owner, self._class_method)
        return getattr(obj, self._instance_method)
