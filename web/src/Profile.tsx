import type { Client } from "./api";
import type { Me } from "./generated";
import { PersonalTokensPane } from "./ClientAuth";
import { ProfileAvatar, signInLabel } from "./ProfileMenu";
import {
  Alert,
  Badge,
  Button,
  SettingsRow,
  SettingsSection,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  PageHeading,
  useAction,
  useResource,
  useToast,
} from "./ui";

// Opens an identity-provider page in a new tab. Only https URLs are opened;
// the server only ever supplies operator-configured or known-issuer URLs.
export function openExternal(url: string) {
  try {
    if (new URL(url).protocol !== "https:") return;
  } catch {
    return;
  }
  window.open(url, "_blank", "noopener,noreferrer");
}

// What "Change password" does for this session. Local accounts open the
// in-console dialog; identity-provider accounts are sent to the provider;
// anything else explains on the profile page.
export function startPasswordChange(
  me: Me,
  openLocal: () => void,
  explain: () => void,
) {
  const pc = me.password_change;
  if (pc.mode === "local") openLocal();
  else if (pc.mode === "external" && pc.url) openExternal(pc.url);
  else explain();
}

export function ProfilePage({
  client,
  me,
  loading,
  error,
  principalId,
  changePassword,
  reload,
}: {
  client: Client;
  me?: Me;
  loading: boolean;
  error: unknown;
  principalId?: string;
  changePassword: () => void;
  reload: () => void;
}) {
  const perms = useResource(
    () =>
      principalId
        ? client.call("getEffectivePermissions", {
            query: { principal: principalId, resource: "instance" },
          })
        : Promise.resolve({ data: undefined, etag: null }),
    [client, principalId],
  );
  const others = useAction();
  const toast = useToast();
  if (loading) return <Loading what="your profile" />;
  if (error || !me)
    return (
      <>
        <PageHeading eyebrow="Account" title="My profile" />
        <Failure error={error ?? new Error("Profile unavailable.")} />
      </>
    );
  const pc = me.password_change;
  const allowed =
    perms.value?.data?.actions.filter((a) => a.allowed).map((a) => a.action) ??
    [];
  return (
    <>
      <PageHeading
        eyebrow="Account"
        title="My profile"
        text="Who you are signed in as, how you sign in, and your personal access tokens."
      />
      <SettingsSection title="Identity">
        <SettingsRow label="Name" description="From your sign-in method.">
          <span className="profile-id">
            <ProfileAvatar me={me} fallback={me.subject} size={40} />
            <b>{me.display_name}</b>
          </span>
        </SettingsRow>
        {me.email && (
          <SettingsRow label="Email">
            <span>{me.email}</span>
          </SettingsRow>
        )}
        <SettingsRow label="Subject" description="Your identifier here.">
          <span className="mono">{me.subject}</span>
        </SettingsRow>
        <SettingsRow label="Signed in with">
          <Badge tone="neutral">{signInLabel(me)}</Badge>
        </SettingsRow>
        {me.session_expires_at && (
          <SettingsRow label="Session expires">
            <span>{new Date(me.session_expires_at).toLocaleString()}</span>
          </SettingsRow>
        )}
      </SettingsSection>
      <SettingsSection title="Roles and permissions">
        <SettingsRow label="Roles">
          {me.roles.length ? (
            <span className="profile-badges">
              {me.roles.map((r) => (
                <Badge key={r.id} tone="neutral">
                  {r.name}
                </Badge>
              ))}
            </span>
          ) : (
            <span className="muted">No roles assigned.</span>
          )}
        </SettingsRow>
        <SettingsRow
          label="Instance permissions"
          description="What your roles allow on the instance."
        >
          {allowed.length ? (
            <span className="profile-badges">
              {allowed.map((a) => (
                <Badge key={a} tone="good">
                  {a}
                </Badge>
              ))}
            </span>
          ) : (
            <span className="muted">
              {perms.loading ? "Loading…" : "None at instance level."}
            </span>
          )}
        </SettingsRow>
      </SettingsSection>
      {me.auth_method !== "token" && (
        <SettingsSection title="Password">
          {pc.mode === "unavailable" && (
            <Alert tone="info" title="Password managed elsewhere">
              {pc.reason ||
                "Your password is managed by your sign-in provider."}
            </Alert>
          )}
          {pc.mode === "local" && (
            <SettingsRow
              label="Password"
              description="Changing it signs out your other sessions."
            >
              <Button variant="secondary" icon="lock" onClick={changePassword}>
                Change password
              </Button>
            </SettingsRow>
          )}
          {pc.mode === "external" && (
            <SettingsRow
              label="Password"
              description={`Managed by ${signInLabel(me)}. It opens in a new tab.`}
            >
              <Button
                variant="secondary"
                icon="external-link"
                onClick={changePassword}
              >
                Change password at {signInLabel(me)}
              </Button>
            </SettingsRow>
          )}
        </SettingsSection>
      )}
      {me.auth_method !== "token" && (
        <SettingsSection title="Sessions">
          <SettingsRow
            label="Signed-in sessions"
            description={`${me.active_sessions} open, including this one.`}
          >
            <Button
              variant="secondary"
              disabled={others.busy || me.active_sessions < 2}
              onClick={async () => {
                const ok = await others.run(() =>
                  client.call("revokeMyOtherSessions"),
                );
                if (ok) {
                  toast("Other sessions signed out");
                  reload();
                }
              }}
            >
              Sign out other sessions
            </Button>
          </SettingsRow>
          <Failure error={others.error} />
        </SettingsSection>
      )}
      <div className="profile-tokens">
        <PersonalTokensPane client={client} />
      </div>
    </>
  );
}
