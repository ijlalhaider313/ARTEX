"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * Top-bar "new version available" indicator: checked once on full page load,
 * lights up next to the version number when an update is available, click to
 * jump straight to the "Version & Updates" card on the system settings page.
 *
 * The backend caches GitHub query results for 30 minutes, so querying on every
 * mount is safe — the unauthenticated GitHub API only allows 60 requests/hour/IP,
 * and without that cache layer, opening a few extra tabs would exhaust the
 * quota, after which an actual desired update could no longer be checked.
 *
 * Query failures are always silent: the top bar is not the place for error
 * toasts; the user can go to the settings page and click "Check for updates"
 * to see the reason.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already incorporates the "version number is comparable" check; dev builds will not light this indicator.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silent: no network / GitHub rate limiting should not surface as an error toast in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`New version ${latest} available, click to update`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Pulsing dot: the top bar has many elements, plain text is easy to miss; the animation makes it visible at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">New version {latest}</span>
      <span className="sm:hidden">New version</span>
    </Link>
  );
}
