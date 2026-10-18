import { useAllFeeds, useAllSources } from "./data";
import type { Client } from "./api";
import type { IdentityRoleGrant } from "./generated";
import { Button, Checkbox, Input, Select } from "./ds/bundle";
import { chk, val } from "./ui";

export const actionList: IdentityRoleGrant["actions"] = [
  "read",
  "publish",
  "delete",
  "configure",
  "replicate",
  "admin",
];

export function Actions({
  value,
  onChange,
  legend = "Actions (select at least one)",
}: {
  value: IdentityRoleGrant["actions"];
  onChange: (v: IdentityRoleGrant["actions"]) => void;
  legend?: string;
}) {
  return (
    <fieldset className="plain actions">
      <legend>{legend}</legend>
      <div className="row wrap">
        {actionList.map((a) => (
          <Checkbox
            key={a}
            label={a}
            checked={value.includes(a)}
            onChange={chk((on) =>
              onChange(on ? [...value, a] : value.filter((x) => x !== a)),
            )}
          />
        ))}
      </div>
    </fieldset>
  );
}

// Feed and source selectors are pickers over what this principal can list,
// never raw UUID inputs.
export function GrantEditor({
  client,
  grants,
  onChange,
}: {
  client: Client;
  grants: IdentityRoleGrant[];
  onChange: (g: IdentityRoleGrant[]) => void;
}) {
  const feeds = useAllFeeds(client);
  const sources = useAllSources(client);
  const update = (i: number, g: IdentityRoleGrant) =>
    onChange(grants.map((v, n) => (n === i ? g : v)));
  const feedOptions = (current?: string) => [
    { value: "", label: "Any feed" },
    ...feeds.items.map((f) => ({ value: f.id, label: f.slug })),
    ...(current && !feeds.items.some((f) => f.id === current)
      ? [{ value: current, label: current }]
      : []),
  ];
  const sourceOptions = (current?: string) => [
    { value: "", label: "Any source" },
    ...sources.items.map((s) => ({
      value: s.id,
      label: `${s.name} (${s.format})`,
    })),
    ...(current && !sources.items.some((s) => s.id === current)
      ? [{ value: current, label: current }]
      : []),
  ];
  return (
    <>
      {grants.map((g, i) => (
        <fieldset className="rulebox" key={i}>
          <legend>Grant {i + 1}</legend>
          <div className="cols">
            <Select
              label="Resource kind"
              options={["artifact", "feed", "source", "instance"]}
              value={g.kind}
              onChange={val((kind) =>
                update(i, {
                  kind: kind as IdentityRoleGrant["kind"],
                  effect: g.effect,
                  actions: g.actions,
                }),
              )}
            />
            <Select
              label="Effect"
              options={["allow", "deny"]}
              value={g.effect}
              onChange={val((effect) =>
                update(i, { ...g, effect: effect as "allow" | "deny" }),
              )}
            />
          </div>
          {(g.kind === "artifact" || g.kind === "feed") && (
            <Select
              label="Feed"
              options={feedOptions(g.feed_id)}
              value={g.feed_id ?? ""}
              onChange={val((v) =>
                update(i, {
                  ...g,
                  feed_id: v || undefined,
                  source_id: undefined,
                }),
              )}
            />
          )}
          {(g.kind === "source" || g.kind === "artifact") && (
            <Select
              label="Source"
              options={sourceOptions(g.source_id)}
              value={g.source_id ?? ""}
              onChange={val((v) =>
                update(i, {
                  ...g,
                  source_id: v || undefined,
                  feed_id: undefined,
                }),
              )}
            />
          )}
          {g.kind === "artifact" && (
            <Input
              label="Namespace (optional)"
              mono
              maxLength={200}
              value={g.namespace ?? ""}
              onChange={val((v) =>
                update(i, { ...g, namespace: v || undefined }),
              )}
            />
          )}
          <Actions
            value={g.actions}
            onChange={(actions) => update(i, { ...g, actions })}
          />
          <Button
            type="button"
            size="sm"
            variant="ghost"
            icon="trash-2"
            onClick={() => onChange(grants.filter((_, n) => n !== i))}
          >
            Remove grant {i + 1}
          </Button>
        </fieldset>
      ))}
      <Button
        type="button"
        size="sm"
        variant="secondary"
        icon="plus"
        disabled={grants.length >= 256}
        onClick={() =>
          onChange([
            ...grants,
            { kind: "artifact", effect: "allow", actions: ["read"] },
          ])
        }
      >
        Add grant
      </Button>
      <p className="muted small">
        Empty scope fields broaden a grant. Deny wins over allow. Instance admin
        grants enable identity administration; review changes before saving.
      </p>
    </>
  );
}
