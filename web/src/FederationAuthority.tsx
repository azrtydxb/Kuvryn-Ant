import { useState } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type {
  FederationAuthorityStatus,
  FederationMember,
  FederationStatus,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Modal,
  PropertyList,
  Select,
  Switch,
  Table,
  Textarea,
} from "./ds/bundle";
import {
  Copyable,
  Failure,
  Loading,
  Mono,
  SectionHead,
  chk,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

type ActionState = ReturnType<typeof useAction>;

// Authority epochs, planned handover, forced recovery and the recovery-key
// drill. Recovery and the drill need the offline recovery key: the console
// shows what to sign, never asks for the key itself.
export function FederationAuthority({
  client,
  st,
  members,
  changed,
}: {
  client: Client;
  st: FederationStatus;
  members: FederationMember[];
  changed: () => void;
}) {
  const authority = useOptionalCall(client, "getFederationAuthority");
  const [dialog, setDialog] = useState<
    "handover" | "recover" | "drill" | undefined
  >();
  const isAuthority = st.role === "authority";
  const reload = () => {
    authority.reload();
    changed();
  };
  if (authority.state === "unavailable") return null;
  const data = authority.state === "ready" ? authority.data : undefined;
  const candidates = members.filter((m) => m.status === "active" && !m.self);
  return (
    <>
      <SectionHead title="Authority" count={data?.epochs.length}>
        {isAuthority && (
          <>
            <Button
              size="sm"
              variant="secondary"
              icon="shield-check"
              onClick={() => setDialog("drill")}
            >
              Recovery-key drill
            </Button>
            <Button
              size="sm"
              variant="danger"
              icon="repeat"
              disabled={!candidates.length}
              onClick={() => setDialog("handover")}
            >
              Hand over
            </Button>
          </>
        )}
        {!isAuthority && (
          <Button
            size="sm"
            variant="danger"
            icon="life-buoy"
            onClick={() => setDialog("recover")}
          >
            Recover authority here
          </Button>
        )}
      </SectionHead>
      {authority.state === "loading" && <Loading what="authority" />}
      {authority.state === "error" && <Failure error={authority.error} />}
      {data?.handover && (
        <Alert tone="warn" title="Handover in progress">
          Publishing is paused while {data.handover.to_name ?? "the target"}{" "}
          takes over (started {when(data.handover.started_at)}, expires{" "}
          {when(data.handover.expires_at)}).
          {isAuthority && (
            <>
              {" "}
              <AbortHandover client={client} done={reload} />
            </>
          )}
        </Alert>
      )}
      {data?.fenced && (
        <Alert tone="warn" title="This instance was fenced">
          It was the authority and learned of epoch {data.fenced.by_epoch} on{" "}
          {when(data.fenced.at)}. Federation administration here is read-only;
          republish anything changed after the epoch change on the new
          authority.
        </Alert>
      )}
      {data && isAuthority && data.recovery.due && (
        <Alert tone="info" title="Recovery key not proven recently">
          {data.recovery.verified_at
            ? `Last proven ${when(data.recovery.verified_at)}.`
            : "Possession has never been proven."}{" "}
          Run the recovery-key drill so a forced recovery is possible when it is
          needed.
        </Alert>
      )}
      {data && (
        <>
          <PropertyList
            items={[
              {
                label: "Epoch",
                value: (
                  <>
                    {data.epoch} · {data.authority_name ?? "—"}{" "}
                    <Mono muted>key {data.key_fingerprint}</Mono>
                  </>
                ),
              },
              {
                label: "Recovery key",
                value: (
                  <>
                    <Mono>{data.recovery_key_fingerprint}</Mono>
                    {data.recovery.verified_at && (
                      <> · proven {when(data.recovery.verified_at)}</>
                    )}
                  </>
                ),
              },
            ]}
          />
          <Table
            columns={[
              { key: "epoch", label: "Epoch", mono: true },
              {
                key: "name",
                label: "Authority",
                render: (e: FederationAuthorityStatus["epochs"][number]) =>
                  e.name ?? e.instance_id,
              },
              {
                key: "certified_by",
                label: "Certified by",
                render: (e: FederationAuthorityStatus["epochs"][number]) => (
                  <Badge
                    tone={e.certified_by === "recovery" ? "warn" : "neutral"}
                  >
                    {e.certified_by === "recovery"
                      ? "recovery key"
                      : "previous epoch"}
                  </Badge>
                ),
              },
              { key: "from_generation", label: "From generation", mono: true },
              {
                key: "applied_at",
                label: "Applied",
                render: (e: FederationAuthorityStatus["epochs"][number]) =>
                  when(e.applied_at),
              },
            ]}
            rows={data.epochs}
          />
        </>
      )}
      {dialog === "handover" && (
        <HandoverModal
          client={client}
          candidates={candidates}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
      {dialog === "recover" && (
        <RecoverModal
          client={client}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
      {dialog === "drill" && (
        <DrillModal
          client={client}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
    </>
  );
}

function AbortHandover({ client, done }: { client: Client; done: () => void }) {
  const action = useAction();
  const toast = useToast();
  return (
    <Button
      size="sm"
      variant="secondary"
      disabled={action.busy}
      onClick={() =>
        void action
          .run(() => client.call("abortFederationAuthorityHandover", {}))
          .then((ok) => {
            if (ok) {
              toast("Handover aborted");
              done();
            }
          })
      }
    >
      Abort handover
    </Button>
  );
}

function ActionModal({
  title,
  description,
  submit,
  valid,
  action,
  onClose,
  onSubmit,
  danger,
  children,
}: {
  title: string;
  description: string;
  submit: string;
  valid: boolean;
  action: ActionState;
  onClose: () => void;
  onSubmit: () => void;
  danger?: boolean;
  children: React.ReactNode;
}) {
  return (
    <Modal
      title={title}
      description={description}
      onClose={onClose}
      width={680}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant={danger ? "danger" : undefined}
            disabled={action.busy || !valid}
            onClick={onSubmit}
          >
            {action.busy ? "Working…" : submit}
          </Button>
        </>
      }
    >
      <Failure error={action.error} />
      {children}
    </Modal>
  );
}

function HandoverModal({
  client,
  candidates,
  onClose,
  done,
}: {
  client: Client;
  candidates: FederationMember[];
  onClose: () => void;
  done: () => void;
}) {
  const [target, setTarget] = useState(candidates[0]?.instance_id ?? "");
  const [typed, setTyped] = useState("");
  const action = useAction();
  const toast = useToast();
  const chosen = candidates.find((m) => m.instance_id === target);
  return (
    <ActionModal
      title="Hand over the authority"
      description="The chosen member generates its own epoch key; this authority certifies it, hands over the federation CA and becomes a plain member. Publishing is paused until it completes."
      submit="Hand over"
      danger
      valid={!!chosen && typed === chosen.name}
      action={action}
      onClose={onClose}
      onSubmit={() =>
        void action
          .run(() =>
            client.call("handoverFederationAuthority", {
              body: { to_member_id: target, confirm: true },
            }),
          )
          .then((ok) => {
            if (ok) {
              toast("Authority handed over");
              done();
            }
          })
      }
    >
      <Select
        label="New authority"
        options={candidates.map((m) => ({
          value: m.instance_id,
          label: m.name,
        }))}
        value={target}
        onChange={val(setTarget)}
      />
      <Input
        label={`Type ${chosen?.name ?? "the member name"} to confirm`}
        mono
        value={typed}
        onChange={val(setTyped)}
      />
    </ActionModal>
  );
}

// RecoverModal is the forced recovery on the member that takes over: fetch the
// template, sign it offline with the recovery key, paste the result.
function RecoverModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: () => void;
}) {
  const [template, setTemplate] = useState<string>();
  const [request, setRequest] = useState("");
  const [removePrev, setRemovePrev] = useState(false);
  const action = useAction();
  const toast = useToast();
  let parsed: { authority_certificate?: unknown } | undefined;
  try {
    parsed = request ? JSON.parse(request) : undefined;
  } catch {
    parsed = undefined;
  }
  const valid = !!parsed?.authority_certificate;
  return (
    <ActionModal
      title="Recover the authority on this member"
      description="Use when the authority is lost or compromised. The recovery key stays offline: this member produces a template, you sign it elsewhere, and paste the result."
      submit="Recover authority"
      danger
      valid={valid}
      action={action}
      onClose={onClose}
      onSubmit={() =>
        void action
          .run(() =>
            client.call("recoverFederationAuthority", {
              body: {
                authority_certificate: parsed!.authority_certificate as never,
                remove_previous_authority: removePrev,
                confirm: true,
              },
            }),
          )
          .then((ok) => {
            if (ok) {
              toast("This member is now the authority");
              done();
            }
          })
      }
    >
      <p>
        1. Generate the template on this member:{" "}
        <Button
          size="sm"
          variant="secondary"
          disabled={action.busy}
          onClick={() =>
            void action
              .run(async () => {
                const t = await client.call(
                  "createFederationRecoveryEpochTemplate",
                  {},
                );
                setTemplate(JSON.stringify(t, null, 2));
              })
              .then(() => undefined)
          }
        >
          Get template
        </Button>
      </p>
      {template && (
        <>
          <Copyable text={template} label="Copy template" />
          <p>
            2. On the machine that holds the recovery key:{" "}
            <Mono>
              ant federation recovery sign-authority-cert --template
              template.json --key recovery.key &gt; request.json
            </Mono>
          </p>
        </>
      )}
      <Textarea
        label="3. Signed request (request.json)"
        mono
        rows={6}
        value={request}
        onChange={val(setRequest)}
      />
      <Switch
        label="Remove the previous authority and revoke its certificate (key compromise)"
        labelPosition="end"
        checked={removePrev}
        onChange={chk(setRemovePrev)}
      />
    </ActionModal>
  );
}

function DrillModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: () => void;
}) {
  const [challenge, setChallenge] = useState<string>();
  const [proof, setProof] = useState("");
  const action = useAction();
  const toast = useToast();
  let parsed: { challenge?: string; signature?: string } | undefined;
  try {
    parsed = proof ? JSON.parse(proof) : undefined;
  } catch {
    parsed = undefined;
  }
  const valid = !!parsed?.challenge && !!parsed?.signature;
  return (
    <ActionModal
      title="Recovery-key drill"
      description="Prove possession of the offline recovery key without changing anything. The challenge is valid for one hour."
      submit="Verify proof"
      valid={valid}
      action={action}
      onClose={onClose}
      onSubmit={() =>
        void action
          .run(() =>
            client.call("verifyFederationRecoveryKey", {
              body: {
                challenge: parsed!.challenge!,
                signature: parsed!.signature!,
              },
            }),
          )
          .then((ok) => {
            if (ok) {
              toast("Recovery key proven");
              done();
            }
          })
      }
    >
      <p>
        1. Request a challenge:{" "}
        <Button
          size="sm"
          variant="secondary"
          disabled={action.busy}
          onClick={() =>
            void action
              .run(async () => {
                const c = await client.call(
                  "createFederationRecoveryChallenge",
                  {},
                );
                setChallenge(JSON.stringify(c, null, 2));
              })
              .then(() => undefined)
          }
        >
          Get challenge
        </Button>
      </p>
      {challenge && (
        <>
          <Copyable text={challenge} label="Copy challenge" />
          <p>
            2. Offline:{" "}
            <Mono>
              ant federation recovery sign-challenge --challenge challenge.json
              --key recovery.key &gt; proof.json
            </Mono>
          </p>
        </>
      )}
      <Textarea
        label="3. Proof (proof.json)"
        mono
        rows={5}
        value={proof}
        onChange={val(setProof)}
      />
    </ActionModal>
  );
}
