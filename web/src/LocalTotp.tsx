import { useState } from "react";
import type { Client } from "./api";
import { Button, Input, Modal } from "./ds/bundle";
import { Copyable, Failure, useAction, useToast, val } from "./ui";

// Enrols the signed-in local account's own TOTP second factor. The otpauth URI
// carries the secret, so it is shown once and never re-fetched.
export function LocalTotpModal({
  client,
  username,
  onClose,
  done,
}: {
  client: Client;
  username: string;
  onClose: () => void;
  done: () => void;
}) {
  const toast = useToast();
  const begin = useAction();
  const confirm = useAction();
  const [uri, setUri] = useState("");
  const [code, setCode] = useState("");
  return (
    <Modal
      title={`Two-factor sign-in for ${username}`}
      description="Add the account to an authenticator app, then enter the 6-digit code to turn it on. Sign-in then asks for a code after the password."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          {uri ? (
            <Button
              disabled={confirm.busy || code.length !== 6}
              onClick={async () => {
                if (
                  await confirm.run(() =>
                    client.call("confirmLocalTotp", { body: { code } }),
                  )
                ) {
                  toast("Two-factor enabled", username);
                  done();
                }
              }}
            >
              Enable two-factor
            </Button>
          ) : (
            <Button
              disabled={begin.busy}
              onClick={async () => {
                await begin.run(async () => {
                  const r = await client.call("beginLocalTotp");
                  setUri(r.data.otpauth_uri);
                });
              }}
            >
              Generate secret
            </Button>
          )}
        </>
      }
    >
      {uri && (
        <div className="stack">
          <Copyable text={uri} label="otpauth URI" />
          <Input
            label="Code"
            inputMode="numeric"
            pattern="[0-9]{6}"
            maxLength={6}
            required
            value={code}
            onChange={val(setCode)}
            autoComplete="one-time-code"
          />
        </div>
      )}
      <Failure error={begin.error ?? confirm.error} />
    </Modal>
  );
}
