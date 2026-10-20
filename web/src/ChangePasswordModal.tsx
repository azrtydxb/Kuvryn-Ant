import { useState } from "react";
import { APIError, type Client } from "./api";
import { Alert, Button, Input, Modal } from "./ds/bundle";
import { Failure, useAction } from "./ui";

// Mirrors the server policy (localadmin.ValidatePassword): length only.
export const MIN_PASSWORD = 14;
export const MAX_PASSWORD = 256;

export function passwordProblems(
  current: string,
  next: string,
  confirm: string,
): string[] {
  const out: string[] = [];
  if (next.length < MIN_PASSWORD)
    out.push(`At least ${MIN_PASSWORD} characters (${next.length} so far)`);
  if (next.length > MAX_PASSWORD)
    out.push(`At most ${MAX_PASSWORD} characters`);
  if (next && current && next === current)
    out.push("Different from the current password");
  if (confirm !== next) out.push("Confirmation matches the new password");
  return out;
}

function message(error: unknown): string | undefined {
  if (!(error instanceof APIError)) return undefined;
  if (error.status === 403) return "The current password is incorrect.";
  if (error.status === 429)
    return "Too many attempts. The account is temporarily locked; wait and try again.";
  return undefined;
}

export function ChangePasswordModal({
  client,
  onClose,
  onChanged,
}: {
  client: Client;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const action = useAction();
  const problems = passwordProblems(current, next, confirm);
  const valid = current !== "" && problems.length === 0;
  const submit = async () => {
    const ok = await action.run(() =>
      client.call("changeMyPassword", {
        body: { current_password: current, new_password: next },
      }),
    );
    // Never keep a rejected current password around; keep the new one so a
    // typo in the current password is a one-field retry.
    setCurrent("");
    if (ok) {
      setNext("");
      setConfirm("");
      onChanged();
    }
  };
  const friendly = message(action.error);
  return (
    <Modal
      title="Change password"
      description="Your other signed-in sessions will be signed out. This session stays signed in."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={!valid || action.busy} onClick={submit}>
            Change password
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (valid && !action.busy) void submit();
        }}
      >
        <Input
          label="Current password"
          type="password"
          autoComplete="current-password"
          value={current}
          maxLength={MAX_PASSWORD}
          onChange={(e: { target: { value: string } }) =>
            setCurrent(e.target.value)
          }
        />
        <Input
          label="New password"
          type="password"
          autoComplete="new-password"
          value={next}
          maxLength={MAX_PASSWORD + 1}
          onChange={(e: { target: { value: string } }) =>
            setNext(e.target.value)
          }
        />
        <Input
          label="Confirm new password"
          type="password"
          autoComplete="new-password"
          value={confirm}
          onChange={(e: { target: { value: string } }) =>
            setConfirm(e.target.value)
          }
        />
        <ul className="pw-policy" aria-label="Password requirements">
          {[
            [
              `At least ${MIN_PASSWORD} characters`,
              next.length >= MIN_PASSWORD,
            ],
            [
              "Different from the current password",
              next !== "" && next !== current,
            ],
            ["Confirmation matches", confirm !== "" && confirm === next],
          ].map(([label, ok]) => (
            <li key={label as string} className={ok ? "is-ok" : ""}>
              {ok ? "✓" : "○"} {label as string}
            </li>
          ))}
        </ul>
        {friendly ? (
          <Alert tone="bad">{friendly}</Alert>
        ) : (
          <Failure error={action.error} />
        )}
      </form>
    </Modal>
  );
}
