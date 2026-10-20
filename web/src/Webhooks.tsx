import { useState } from "react";
import type { Client } from "./api";
import type { Go } from "./App";
import { Tabs } from "./ds/bundle";
import { IncomingHooks } from "./IncomingHooks";
import { OutgoingWebhooks } from "./OutgoingWebhooks";
import { PageHeading } from "./ui";

export type WebhookTab = "incoming" | "outgoing";

/**
 * Webhooks in both directions. Incoming hooks are URLs on this instance that
 * external systems call; outgoing webhooks are signed deliveries of Ant events
 * to other endpoints. Both use the signing secrets under Access.
 */
export function Webhooks({
  client,
  go,
  initialTab = "incoming",
}: {
  client: Client;
  go?: Go;
  initialTab?: WebhookTab;
}) {
  const [tab, setTab] = useState<WebhookTab>(initialTab);
  const [slot, setSlot] = useState<HTMLElement | null>(null);
  return (
    <>
      <PageHeading
        eyebrow="Integrations"
        title="Webhooks"
        text="Receive calls from registries, forges and CI, and deliver signed events to your own endpoints."
      >
        <div className="row" ref={setSlot} />
      </PageHeading>
      <Tabs
        value={tab}
        onChange={(t: string) => setTab(t as WebhookTab)}
        className="spaced-tabs"
        items={[
          { id: "incoming", label: "Incoming" },
          { id: "outgoing", label: "Outgoing" },
        ]}
      />
      {tab === "incoming" ? (
        <IncomingHooks client={client} go={go} slot={slot} />
      ) : (
        <OutgoingWebhooks client={client} go={go} slot={slot} />
      )}
    </>
  );
}
