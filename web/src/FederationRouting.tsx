import { useState } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type {
  FederationFeedRouting,
  FederationRouteCandidate,
  FederationRoutePlan,
  FederationTopologyFeedView,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  Input,
  Modal,
  Select,
  Table,
} from "./ds/bundle";
import { chk, Failure, Loading, Mono, useAction, val } from "./ui";

const decisionText: Record<string, string> = {
  local: "Served locally (this member is the home)",
  proxy: "Proxied to another member",
  redirect: "Redirected to another member",
  off: "Routing off: local content only",
  no_candidates: "No other member holds this feed",
  unavailable: "No holder is eligible: reads answer 503",
};

/** Short routing summary for tables. */
export function routingSummary(r?: FederationFeedRouting) {
  if (!r) return "proxy";
  const flags = [r.blob_urls && "blob URLs", r.cache && "cache"].filter(
    Boolean,
  );
  return flags.length ? `${r.mode} · ${flags.join(", ")}` : r.mode;
}

/** The ordered members a read that misses locally would go to. */
export function RouteCandidates({ plan }: { plan: FederationRoutePlan }) {
  return (
    <>
      <Alert
        tone={plan.decision === "unavailable" ? "warn" : "info"}
        title={decisionText[plan.decision] ?? plan.decision}
      >
        {plan.reason}
      </Alert>
      {plan.candidates.length > 0 && (
        <Table
          columns={[
            {
              key: "order",
              label: "#",
              align: "right",
              render: (c: FederationRouteCandidate) =>
                c.order ? c.order : "—",
            },
            { key: "name", label: "Member" },
            {
              key: "role",
              label: "Role",
              render: (c: FederationRouteCandidate) => (
                <span className="row tight">
                  <Badge tone={c.role === "home" ? "info" : undefined}>
                    {c.role}
                  </Badge>
                  {c.replica_state && (
                    <Badge
                      tone={
                        c.replica_state === "in_sync"
                          ? "good"
                          : c.replica_state === "unknown"
                            ? "neutral"
                            : "warn"
                      }
                    >
                      {c.replica_state.replace("_", " ")}
                    </Badge>
                  )}
                </span>
              ),
            },
            {
              key: "circuit",
              label: "Circuit",
              render: (c: FederationRouteCandidate) => (
                <Badge
                  tone={
                    c.circuit === "closed"
                      ? "good"
                      : c.circuit === "open"
                        ? "bad"
                        : "warn"
                  }
                >
                  {c.circuit.replace("_", " ")}
                </Badge>
              ),
            },
            {
              key: "rtt_ms",
              label: "RTT",
              align: "right",
              render: (c: FederationRouteCandidate) =>
                c.rtt_ms != null ? `${Math.round(c.rtt_ms)} ms` : "—",
            },
            { key: "reason", label: "Reason" },
          ]}
          rows={plan.candidates}
        />
      )}
    </>
  );
}

/** Explain where reads of a feed key go from this member. */
export function RouteModal({
  client,
  feedKey,
  onClose,
}: {
  client: Client;
  feedKey: string;
  onClose: () => void;
}) {
  const [fed, slug] = feedKey.split("/");
  const plan = useOptionalCall(client, "explainFederationRoute", {
    params: { fed, slug },
  });
  return (
    <Modal
      title={`Read routing of ${feedKey}`}
      description="Where a read that misses on this member goes. Writes are never routed: they go to the home only."
      onClose={onClose}
      width={820}
      actions={<Button onClick={onClose}>Done</Button>}
    >
      {plan.state === "loading" && <Loading what="route" />}
      {plan.state === "error" && <Failure error={plan.error} />}
      {plan.state === "ready" && (
        <>
          <p className="muted">
            <Mono>{plan.data.feed_key}</Mono> ·{" "}
            {routingSummary(plan.data.routing)} · on this member:{" "}
            {plan.data.local_role}
          </p>
          <RouteCandidates plan={plan.data} />
        </>
      )}
    </Modal>
  );
}

/** Authority: set the routing of a feed key (published with the topology). */
export function RoutingModal({
  client,
  feed,
  onClose,
  done,
}: {
  client: Client;
  feed: FederationTopologyFeedView;
  onClose: () => void;
  done: () => void;
}) {
  const r = feed.routing;
  const [mode, setMode] = useState<string>(r?.mode ?? "proxy");
  const [blobURLs, setBlobURLs] = useState(r?.blob_urls ?? false);
  const [cache, setCache] = useState(r?.cache ?? false);
  const [ttl, setTTL] = useState(
    r?.cache_ttl_seconds ? String(r.cache_ttl_seconds) : "",
  );
  const [maxLag, setMaxLag] = useState(
    r?.max_lag_seconds ? String(r.max_lag_seconds) : "",
  );
  const [prefer, setPrefer] = useState((r?.prefer ?? []).join(", "));
  const action = useAction();
  const [fed, slug] = feed.key.split("/");
  const proxy = mode === "proxy";
  return (
    <Modal
      title={`Routing of ${feed.key}`}
      description="How members that miss content of this feed reach a member that holds it. Proxy is the default and works with every client."
      onClose={onClose}
      width={600}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={action.busy}
            onClick={async () => {
              const body: FederationFeedRouting = {
                mode: mode as FederationFeedRouting["mode"],
                blob_urls: proxy && blobURLs,
                cache: proxy && cache,
              };
              if (proxy && cache && ttl) body.cache_ttl_seconds = Number(ttl);
              if (maxLag) body.max_lag_seconds = Number(maxLag);
              const keys = prefer
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean);
              if (keys.length) body.prefer = keys;
              if (
                await action.run(() =>
                  client.call("setFederationFeedRouting", {
                    params: { fed, slug },
                    body,
                  }),
                )
              )
                done();
            }}
          >
            Publish routing
          </Button>
        </>
      }
    >
      <Failure error={action.error} />
      <Select
        label="Mode"
        value={mode}
        onChange={val(setMode)}
        options={[
          { value: "proxy", label: "Proxy through the entry member (default)" },
          { value: "redirect", label: "Redirect to the holder (307)" },
          { value: "off", label: "Off: local content only" },
        ]}
      />
      {mode === "redirect" && (
        <Alert tone="warn" title="Clients must authenticate at the holder">
          Redirects only work for anonymous feeds or clients with credentials
          for the holding member. Members holding a replica still proxy their
          misses.
        </Alert>
      )}
      {proxy && (
        <>
          <Checkbox
            label="Signed blob URLs"
            description="Immutable blob reads get a 307 to a signed, five-minute URL on the holder's public listener instead of streaming through the entry member."
            checked={blobURLs}
            onChange={chk(setBlobURLs)}
          />
          <Checkbox
            label="Cache on entry members"
            description="Members with a feed bound to this key keep immutable proxied responses for callers who may read their whole feed."
            checked={cache}
            onChange={chk(setCache)}
          />
          {cache && (
            <Input
              size="sm"
              label="Cache lifetime (seconds)"
              placeholder="86400"
              value={ttl}
              onChange={val(setTTL)}
            />
          )}
        </>
      )}
      <Input
        size="sm"
        label="Max replica lag (seconds)"
        placeholder="each rule's lag budget"
        value={maxLag}
        onChange={val(setMaxLag)}
      />
      <Input
        size="sm"
        mono
        label="Prefer members sharing labels"
        placeholder="site, region"
        value={prefer}
        onChange={val(setPrefer)}
      />
    </Modal>
  );
}
