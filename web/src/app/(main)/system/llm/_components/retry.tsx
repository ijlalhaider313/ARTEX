"use client";

// Shared LLM retry configuration: the "count + interval" for each of the five retry layers.
//
// Five layers from inner to outer: connect (SDK) → empty response (SDK) → same-provider safe window → polling breaker → intent rerun.
// The first three travel with the endpoint, so each model config can override the global defaults; the last two are process-level and only have one global copy.
//
// All inputs follow the same "empty = not configured" semantics, matching the backend db.RetryRule:
//   attempts   empty/0 = use built-in default | -1 = disable this layer | >0 = use this count
//   interval   empty/0 = use this layer's original exponential backoff | >0 = use this fixed millisecond interval instead

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** Where this retry layer happens and who runs it */
  where: string;
  /** What kind of errors reach this layer — specific down to status codes, no guessing */
  trigger: string;
  /** Errors that look similar but do [NOT] go through this layer, so people don't fill it in, see no effect, and assume it's a bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default value for the attempts field when empty, used for the placeholder */
  defAttempts: number;
  /** Default interval strategy when empty, used for the placeholder */
  defInterval: string;
  /** Meaning of setting attempts to -1 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connection retry",
    where: "SDK · before a 200 is received",
    trigger:
      "Cannot connect or no 200 yet: connection reset / read-write timeout / DNS failure and other network-layer errors, plus HTTP 408, 429, 500, 502, 503, 504.",
    skips: "Other status codes (400 / 401 / 403 / 404 / 413 / 422 etc.) are deterministic refusals — resending fails the same way, so they bubble up immediately.",
    desc: "Resends the same request verbatim. Once the stream has started (a 200 was received), a mid-stream break is no longer this layer's concern.",
    attemptsLabel: "Retry attempts",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = no retries at all, fail immediately",
  },
  empty: {
    title: "Empty-response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200, finish_reason is a normal stop, but the whole response has no content blocks — gateway empty frames, dropped thinking-field frames, or sampling hiccups all look like this.",
    skips: "Truncation due to max_tokens with no content does not count (fix that by raising the output cap; resending just hits it again).",
    desc: "Resends the entire prompt, so it is expensive on long contexts — keep the count small.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = hand the empty response through as-is",
  },
  stream: {
    title: "Same-provider safe-window retry",
    where: "This project · before any output is delivered",
    trigger:
      "Problems that occur only after the stream is established (a 200 was received): mid-stream disconnect, provider overloaded, in-stream 429 / 5xx error events — and not a single token has been handed to the caller yet.",
    skips:
      "Quota exhausted (402 / insufficient_quota, handled by polling to switch config), context too long (413 / context length, handled by compression), and deterministic refusals 400 / 401 / 403 / 404 / 422 are not retried.",
    desc: "Replays the same request on the same config. Because no output has been delivered yet, replay does not duplicate model output or tool execution.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = a broken stream goes straight to the outer intent-rerun layer",
  },
  breaker: {
    title: "Polling breaker",
    where: "This project · process-level, one global copy",
    trigger:
      "Transient failures (429, 5xx, network errors) trip the breaker once they accumulate to the threshold; deterministic failures like insufficient quota (402), invalid key (401 / 403), or model not found (404) ignore the threshold and trip on the first occurrence.",
    skips: "One success resets the counter, so an occasionally flaky config does not slowly accrue into a trip.",
    desc: "After tripping, the config enters a cooldown during which polling skips it. State is persisted, so restarts do not lose it.",
    attemptsLabel: "Consecutive failures to trip",
    defAttempts: 3,
    defInterval: "1min→5min→30min stepped",
    offHint: "-1 = transient failures never trip (deterministic failures still trip)",
  },
  intent: {
    title: "Intent rerun",
    where: "This project · process-level, one global copy",
    trigger:
      "The inner layers did not catch it: the worker ends with model_error — inner retries are exhausted, or the stream broke after delivery had already started (replay is unsafe then, the whole intent must rerun).",
    skips: "Quota exhaustion is already handled by polling switching config, not rerun here; when a task is paused / terminated / finalizing it yields immediately and does not consume backoff time.",
    desc: "The entire intent runs from the top again. It is the outermost layer, so one rerun means the inner layers' counts multiply through once more.",
    attemptsLabel: "Rerun attempts",
    defAttempts: 2,
    defInterval: "Fixed 3s",
    offHint: "-1 = no rerun, the intent is judged blocked",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Human-readable milliseconds, only for echoing next to an input so you don't count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled numeric input: empty string ↔ 0; intermediate states ("-", "1e") stay local and do not disturb the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // When the parent swaps in a whole new value set (loaded a policy, switched config) we follow;
  // typing does not reach here because by then value already equals the parsed result of local text.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** The two knobs for one retry layer. idPrefix keeps label htmlFor unique when the same layer appears multiple times on a page. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = compact version for the config drawer: drop the expanded explanation, keep only the "what errors reach this layer" line */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* Which errors reach this layer, down to status codes — if you set the knobs but see no effect, the error probably does not fall into this layer at all. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Trigger</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Does not hit this layer</span>: {meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`Default ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            Interval ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="Default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `Fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Empty = use default; {meta.offHint}.</p>}
    </div>
  );
}

/** The three-layer override in the model config drawer (the three layers that travel with the endpoint). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">Retry overrides</Label>
        <p className="text-muted-foreground text-xs">
          Applies only to this config, overriding the global defaults in "Retry & backoff". Leave any field empty to follow the global setting; set attempts to -1 to disable that layer;
          a filled interval replaces exponential backoff with a fixed interval. Breaker and intent rerun are process-level and can only be tuned on the global page.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** "Retry & backoff" tab: global defaults for all five layers. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`Failed to read retry policy: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend clamps out-of-range values back into range and returns them; refresh with the returned value so what you see is what is stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved, effective immediately (the current in-flight call still uses the old parameters)");
    } catch (e) {
      toast.error(`Save failed: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> Loading retry policy…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A failed model call passes through five retry layers in order, from inner to outer:
        <span className="text-foreground"> connect → empty response → same-provider safe window → polling breaker → intent rerun</span>
        . An outer layer is reached only when the inner ones are exhausted, so the counts
        <span className="text-foreground">multiply</span>
        — maxing out every layer can burn dozens of requests on a single hiccup.
        Leaving everything empty matches the current defaults, identical to the behavior before this page existed. The first three layers can be overridden per model config.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          Save
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          Reset all to defaults
        </Button>
      </div>
    </div>
  );
}
