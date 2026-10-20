import { useEffect, useMemo, useState } from "react";
import type { Client } from "./api";
import { useSession } from "./useSession";
import { SignIn } from "./SignIn";
import { Overview } from "./Overview";
import { PackagesPage, PackageDrawer, type PackageRef } from "./Packages";
import { FeedsPage } from "./Feeds";
import { SourcesPage } from "./Sources";
import { ReplicationPage } from "./Replication";
import { FederationPage } from "./Federation";
import { Identity } from "./Identity";
import { Webhooks } from "./Webhooks";
import { AuditPage } from "./Audit";
import { SettingsPage } from "./Settings";
import { MigrationPage } from "./Migration";
import { ProfilePage, startPasswordChange } from "./Profile";
import { ProfileMenu, type ThemePref } from "./ProfileMenu";
import { ChangePasswordModal } from "./ChangePasswordModal";
import { loadAllFeeds } from "./data";
import {
  Alert,
  Button,
  CommandPalette,
  IconButton,
  Icon,
  Kbd,
  Sidebar,
  Spinner,
  Topbar,
} from "./ds/bundle";
import { NotesContext, ToastProvider, Failure, useResource } from "./ui";
import brand from "./ds/assets/brand/ant-emblem.png";

export type Page =
  | "overview"
  | "packages"
  | "feeds"
  | "sources"
  | "replication"
  | "federation"
  | "identity"
  | "webhooks"
  | "audit"
  | "migration"
  | "settings"
  | "profile";
export type Go = (
  page: Page,
  options?: {
    feed?: string;
    create?: boolean;
    source?: string;
    tab?: string;
  },
) => void;
const NAV: { id: Page; label: string; icon: string }[] = [
  { id: "overview", label: "Overview", icon: "gauge" },
  { id: "packages", label: "Packages", icon: "package" },
  { id: "feeds", label: "Feeds", icon: "rss" },
  { id: "sources", label: "Sources", icon: "database" },
  { id: "replication", label: "Replication", icon: "copy" },
  { id: "federation", label: "Federation", icon: "network" },
  { id: "identity", label: "Access", icon: "shield" },
  { id: "webhooks", label: "Webhooks", icon: "webhook" },
  { id: "audit", label: "Audit log", icon: "scroll-text" },
  { id: "migration", label: "Import & migrate", icon: "arrow-down-up" },
  { id: "settings", label: "Settings", icon: "settings" },
];
type Theme = "light" | "dark";
function systemTheme(): Theme {
  return typeof matchMedia === "function" &&
    matchMedia("(prefers-color-scheme: light)").matches
    ? "light"
    : "dark";
}
function initialPref(): ThemePref {
  try {
    const saved = localStorage.getItem("ant-theme");
    if (saved === "light" || saved === "dark" || saved === "system")
      return saved;
  } catch {
    /* storage unavailable */
  }
  return "system";
}
function initialTheme(): Theme {
  const pref = initialPref();
  return pref === "system" ? systemTheme() : pref;
}

function Shell({
  client,
  sessionLabel,
  signOut,
  signingOut,
  principalId,
}: {
  client: Client;
  principalId?: string;
  sessionLabel: string;
  signOut: () => void;
  signingOut: boolean;
}) {
  // Hide admin-only areas when the server says this principal cannot use them.
  // Unknown (service token, older server, request failed) keeps everything visible.
  const perms = useResource(
    () =>
      principalId
        ? client.call("getEffectivePermissions", {
            query: { principal: principalId, resource: "instance" },
          })
        : Promise.resolve({ data: undefined, etag: null }),
    [client, principalId],
  );
  const can = (action: string) =>
    perms.value?.data?.actions?.find((a) => a.action === action)?.allowed;
  const hidden = new Set<Page>(
    can("admin") === false
      ? [
          "identity",
          "webhooks",
          "migration",
          "settings",
          ...(can("replicate") === false ? (["replication"] as Page[]) : []),
        ]
      : [],
  );
  const nav = NAV.filter((n) => !hidden.has(n.id));
  const [page, setPage] = useState<Page>("overview");
  const [feed, setFeed] = useState("");
  const [crumb, setCrumb] = useState("");
  const [createFeed, setCreateFeed] = useState(false);
  const [openSource, setOpenSource] = useState("");
  const [pageTab, setPageTab] = useState("");
  const [pkg, setPkg] = useState<PackageRef>();
  const [palette, setPalette] = useState(false);
  const [menu, setMenu] = useState(false);
  const [notes, setNotes] = useState(false);
  const [themePref, setThemePrefState] = useState<ThemePref>(initialPref);
  const [systemDark, setSystemTheme] = useState<Theme>(systemTheme);
  const theme: Theme = themePref === "system" ? systemDark : themePref;
  const setThemePref = (t: ThemePref) => {
    setThemePrefState(t);
    try {
      localStorage.setItem("ant-theme", t);
    } catch {
      /* storage unavailable */
    }
  };
  const me = useResource(() => client.call("getMe"), [client]);
  const [pwOpen, setPwOpen] = useState(false);
  useEffect(() => {
    if (typeof matchMedia !== "function") return;
    const q = matchMedia("(prefers-color-scheme: light)");
    const on = () => setSystemTheme(q.matches ? "light" : "dark");
    q.addEventListener?.("change", on);
    return () => q.removeEventListener?.("change", on);
  }, []);
  const rawMe = me.value?.data;
  // Older servers or odd responses: fall back to the plain session label.
  const meData =
    rawMe &&
    Array.isArray(rawMe.roles) &&
    rawMe.password_change &&
    rawMe.auth_method
      ? rawMe
      : undefined;
  const changePassword = () =>
    meData
      ? startPasswordChange(
          meData,
          () => setPwOpen(true),
          () => go("profile"),
        )
      : go("profile");
  const [paletteFeeds, setPaletteFeeds] = useState<
    { id: string; slug: string; format: string }[]
  >([]);
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);
  const go: Go = (p, options = {}) => {
    setPage(p);
    setFeed(options.feed ?? "");
    setCrumb("");
    setCreateFeed(!!options.create);
    setOpenSource(options.source ?? "");
    setPageTab(options.tab ?? "");
    setMenu(false);
  };
  useEffect(() => {
    const heading = document.querySelector<HTMLElement>("main h1");
    if (heading) {
      heading.tabIndex = -1;
      heading.focus({ preventScroll: true });
    }
  }, [page, feed]);
  useEffect(() => {
    const k = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((v) => !v);
      }
    };
    window.addEventListener("keydown", k);
    return () => window.removeEventListener("keydown", k);
  }, []);
  useEffect(() => {
    if (!palette) return;
    let active = true;
    loadAllFeeds(client)
      .then((items) => {
        if (active) setPaletteFeeds(items);
      })
      .catch(() => undefined);
    return () => {
      active = false;
    };
  }, [palette, client]);
  const commands = useMemo(
    () => [
      ...NAV.map((n) => ({
        id: "nav:" + n.id,
        label: n.label,
        group: "Go to",
        icon: n.icon,
      })),
      ...paletteFeeds.map((f) => ({
        id: "feed:" + f.id,
        label: f.slug,
        group: "Feeds",
        icon: "rss",
        hint: f.format,
      })),
      {
        id: "act:create",
        label: "Create feed",
        group: "Actions",
        icon: "plus",
      },
      {
        id: "act:token",
        label: "Issue token",
        group: "Actions",
        icon: "key-round",
      },
    ],
    [paletteFeeds],
  );
  const onCommand = (c: { id: string }) => {
    setPalette(false);
    const [kind, ...rest] = c.id.split(":");
    const value = rest.join(":");
    if (kind === "nav") go(value as Page);
    else if (kind === "feed") go("feeds", { feed: value });
    else if (c.id === "act:create") go("feeds", { create: true });
    else if (c.id === "act:token") go("identity");
  };
  const label =
    page === "profile" ? "My profile" : NAV.find((n) => n.id === page)!.label;
  const crumbs = ["Kuvryn Ant", label, ...(crumb ? [crumb] : [])];
  let body;
  if (page === "overview")
    body = <Overview client={client} go={go} openPackage={setPkg} />;
  else if (page === "packages")
    body = <PackagesPage client={client} openPackage={setPkg} />;
  else if (page === "feeds")
    body = (
      <FeedsPage
        client={client}
        feedId={feed}
        create={createFeed}
        go={go}
        setCrumb={setCrumb}
        openPackage={setPkg}
        closeCreate={() => setCreateFeed(false)}
        startCreate={() => setCreateFeed(true)}
      />
    );
  else if (page === "sources")
    body = <SourcesPage client={client} openId={openSource} go={go} />;
  else if (page === "replication")
    body = (
      <ReplicationPage
        key={pageTab}
        client={client}
        initialTab={
          pageTab === "federation" || pageTab === "peers"
            ? pageTab
            : "replication"
        }
      />
    );
  else if (page === "federation")
    body = <FederationPage client={client} go={go} />;
  else if (page === "identity")
    body = <Identity client={client} initialTab={pageTab || undefined} />;
  else if (page === "webhooks") body = <Webhooks client={client} go={go} />;
  else if (page === "audit") body = <AuditPage client={client} />;
  else if (page === "migration") body = <MigrationPage client={client} />;
  else if (page === "profile")
    body = (
      <ProfilePage
        client={client}
        me={meData}
        loading={me.loading}
        error={me.error}
        principalId={principalId}
        changePassword={changePassword}
        reload={me.reload}
      />
    );
  else
    body = <SettingsPage client={client} notes={notes} setNotes={setNotes} />;
  return (
    <NotesContext.Provider value={notes}>
      <ToastProvider>
        <div className="app">
          <a className="skip" href="#main">
            Skip to content
          </a>
          <div className={"app__side" + (menu ? " is-open" : "")}>
            <Sidebar
              items={nav}
              active={page}
              onSelect={(id: Page) => go(id)}
              brand={
                <button
                  type="button"
                  className="brand"
                  onClick={() => go("overview")}
                >
                  <img src={brand} alt="" width={40} height={40} />
                  <span>
                    <b>Kuvryn</b>
                    <small>Ant</small>
                  </span>
                </button>
              }
              status="Management API v1"
            />
          </div>
          {menu && (
            <button
              type="button"
              className="app__scrim"
              aria-label="Close navigation"
              onClick={() => setMenu(false)}
            />
          )}
          <div className="app__main">
            <Topbar crumbs={crumbs} live={false}>
              <IconButton
                icon="menu"
                label="Open navigation"
                className="app__menu"
                aria-expanded={menu}
                onClick={() => setMenu((v) => !v)}
              />
              <button
                type="button"
                className="search"
                onClick={() => setPalette(true)}
              >
                <Icon name="search" size={15} />
                <span>Search…</span>
                <Kbd>⌘K</Kbd>
              </button>
              <IconButton
                icon={theme === "dark" ? "sun" : "moon"}
                label={
                  theme === "dark"
                    ? "Switch to light theme"
                    : "Switch to dark theme"
                }
                onClick={() =>
                  setThemePref(theme === "dark" ? "light" : "dark")
                }
              />
              <ProfileMenu
                me={meData}
                fallback={sessionLabel}
                themePref={themePref}
                setThemePref={setThemePref}
                openProfile={() => go("profile")}
                openTokens={() => go("profile")}
                changePassword={changePassword}
                signOut={() => {
                  if (!signingOut) signOut();
                }}
              />
            </Topbar>
            <main id="main" tabIndex={-1} className="app__content">
              {body}
              <footer>
                Kuvryn Ant · Management console · All changes are authorized by
                the service.
              </footer>
            </main>
          </div>
          {pwOpen && (
            <ChangePasswordModal
              client={client}
              onClose={() => setPwOpen(false)}
              onChanged={() => {
                setPwOpen(false);
                me.reload();
              }}
            />
          )}
          <PackageDrawer
            client={client}
            pkg={pkg}
            onClose={() => setPkg(undefined)}
            openFeed={(id) => {
              setPkg(undefined);
              go("feeds", { feed: id });
            }}
          />
          <CommandPalette
            open={palette}
            items={commands}
            onSelect={onCommand}
            onClose={() => setPalette(false)}
            placeholder="Search feeds and pages…"
          />
        </div>
      </ToastProvider>
    </NotesContext.Provider>
  );
}

export default function App() {
  const auth = useSession();
  const { client } = auth;
  useEffect(() => {
    document.documentElement.dataset.theme = initialTheme();
  }, []);
  if (client)
    return (
      <Shell
        client={client}
        sessionLabel={
          auth.browser ? "Browser session" : "In-memory service token"
        }
        signOut={() => void auth.signOut()}
        signingOut={auth.loggingOut}
        principalId={auth.browser?.session.PrincipalID}
      />
    );
  return (
    <main id="main" tabIndex={-1} className="signin-page">
      {auth.checking ? (
        <p role="status" className="loading">
          <Spinner size={16} /> Checking your browser session…
        </p>
      ) : auth.loggingOut ? (
        <p role="status" className="loading">
          <Spinner size={16} /> Signing out…
        </p>
      ) : (
        <SignIn
          onSession={auth.accept}
          onToken={auth.useToken}
          notice={
            <>
              <Failure error={auth.error} />
              {auth.logoutFailed && (
                <Alert
                  tone="warn"
                  title="Server sign-out not confirmed"
                  action={
                    <Button
                      size="sm"
                      variant="secondary"
                      onClick={() => void auth.signOut()}
                    >
                      Retry server sign-out
                    </Button>
                  }
                >
                  Local credentials are cleared.
                </Alert>
              )}
              {auth.error && !auth.logoutFailed && (
                <Button variant="secondary" onClick={auth.recheck}>
                  Retry session check
                </Button>
              )}
            </>
          }
        />
      )}
    </main>
  );
}
