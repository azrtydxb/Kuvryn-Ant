import { useEffect, useRef } from "react";
import type { Me } from "./generated";
import { Avatar, DropdownMenu } from "./ds/bundle";

export type ThemePref = "light" | "dark" | "system";

export const METHOD_LABEL: Record<Me["auth_method"], string> = {
  local: "Local password",
  oidc: "Single sign-on",
  saml: "SAML",
  ldap: "LDAP",
  token: "API token",
};

// "Zitadel" for an OIDC session from a provider named Zitadel; otherwise the
// method name. Never trusts the picture/name beyond rendering them as text.
export function signInLabel(me: Me): string {
  if (me.auth_method === "token" || me.auth_method === "local")
    return METHOD_LABEL[me.auth_method];
  return me.idp || METHOD_LABEL[me.auth_method];
}

export function ProfileAvatar({
  me,
  fallback,
  size = 32,
}: {
  me?: Me;
  fallback: string;
  size?: number;
}) {
  const name = me?.display_name || fallback;
  // The picture is the IdP's; a broken or blocked image falls back to initials.
  const src = me?.picture;
  return (
    <Avatar
      name={name}
      src={src}
      size={size}
      key={src ?? "initials"}
      tone="sky"
    />
  );
}

type Item = {
  label?: string;
  icon?: string;
  heading?: string;
  divider?: boolean;
  danger?: boolean;
  disabled?: boolean;
  onSelect?: () => void;
};

export function ProfileMenu({
  me,
  fallback,
  themePref,
  setThemePref,
  openProfile,
  openTokens,
  changePassword,
  signOut,
}: {
  me?: Me;
  fallback: string;
  themePref: ThemePref;
  setThemePref: (t: ThemePref) => void;
  openProfile: () => void;
  openTokens: () => void;
  changePassword: () => void;
  signOut: () => void;
}) {
  const trigger = useRef<HTMLButtonElement>(null);
  const items: Item[] = [];
  if (me) {
    items.push({ heading: me.display_name });
    const who = me.email || me.subject;
    if (who && who !== me.display_name) items.push({ heading: who });
    items.push({ heading: "Signed in with " + signInLabel(me) });
    if (me.roles.length)
      items.push({
        heading: "Roles: " + me.roles.map((r) => r.name).join(", "),
      });
  } else {
    items.push({ heading: fallback });
  }
  items.push({ divider: true });
  items.push({ label: "My profile", icon: "user", onSelect: openProfile });
  items.push({
    label: "Personal access tokens",
    icon: "key-round",
    onSelect: openTokens,
  });
  if (me && me.auth_method !== "token")
    items.push({
      label:
        me.password_change.mode === "external"
          ? "Change password (opens " + signInLabel(me) + ")"
          : "Change password",
      icon: "lock",
      onSelect: changePassword,
    });
  items.push({ divider: true });
  for (const [id, label, icon] of [
    ["light", "Light theme", "sun"],
    ["dark", "Dark theme", "moon"],
    ["system", "Match system theme", "monitor"],
  ] as const)
    items.push({
      label: label + (themePref === id ? "  ✓" : ""),
      icon,
      onSelect: () => setThemePref(id),
    });
  items.push({ divider: true });
  items.push({
    label: "Sign out",
    icon: "log-out",
    danger: true,
    onSelect: signOut,
  });

  // The design-system menu handles open/close, outside click and Escape. Add
  // arrow-key movement between items and return focus to the trigger on close.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const menu = document.querySelector<HTMLElement>('[role="menu"]');
      if (!menu) return;
      const entries = [
        ...menu.querySelectorAll<HTMLButtonElement>(
          '[role="menuitem"]:not(:disabled)',
        ),
      ];
      const at = entries.indexOf(document.activeElement as HTMLButtonElement);
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const step = e.key === "ArrowDown" ? 1 : -1;
        entries[(at + step + entries.length) % entries.length]?.focus();
      } else if (e.key === "Home") {
        e.preventDefault();
        entries[0]?.focus();
      } else if (e.key === "End") {
        e.preventDefault();
        entries[entries.length - 1]?.focus();
      } else if (e.key === "Escape") {
        trigger.current?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  return (
    <DropdownMenu
      align="end"
      items={items}
      className="profile-menu"
      trigger={
        <button
          type="button"
          ref={trigger}
          className="profile-trigger"
          aria-label={"Account menu for " + (me?.display_name || fallback)}
          aria-haspopup="menu"
          onKeyDown={(e) => {
            // Open on ArrowDown like a native menu button, then focus item one.
            if (e.key === "ArrowDown") {
              e.preventDefault();
              e.currentTarget.click();
              setTimeout(
                () =>
                  document
                    .querySelector<HTMLElement>('[role="menuitem"]')
                    ?.focus(),
                0,
              );
            }
          }}
        >
          <ProfileAvatar me={me} fallback={fallback} />
        </button>
      }
    />
  );
}
