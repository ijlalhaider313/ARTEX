"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// Send/newline key bindings for the conversation input. Pure client-side preference: stored only in
// localStorage, not in the database, not synced across accounts — so switching browsers requires
// resetting. See issue #39: 0.3.2 changed Ctrl+Enter-to-send to Enter-to-send; this brings the old
// binding back as an option.
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter to send, Shift+Enter for newline" },
  { value: "ctrl-enter", label: "Ctrl+Enter to send, Enter for newline" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// Subscriber set within the same tab. localStorage's storage event only fires in "other" tabs;
// when this tab changes the setting, emit must notify the input box in the same tab, otherwise a
// refresh is required for it to take effect.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// Returns a string literal; Object.is compares by value, so useSyncExternalStore will not loop.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// No localStorage on the server: render the default first, then correct it in getSnapshot after hydration.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey decides whether a keystroke should send.
// isComposing / keyCode 229 means an IME (e.g. Chinese) is still composing characters — it must be
// allowed through, otherwise pressing Enter to confirm a candidate would send by mistake.
// enter mode excludes only Shift, matching 0.3.2's behavior verbatim — users who don't change the setting feel no difference.
// ctrl-enter mode accepts both Ctrl and Cmd (macOS).
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
