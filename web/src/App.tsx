import { useEffect, useMemo, useState } from "react";
import type { Client } from "./api";
import { useSession } from "./useSession";
import { SignIn } from "./SignIn";
import { Overview } from "./Overview";
import { PackagesPage, PackageDrawer, type PackageRef } from "./Packages";
import { FeedsPage } from "./Feeds";
import { SourcesPage } from "./Sources";
import { Identity } from "./Identity";
import { Webhooks } from "./Webhooks";
import { AuditPage } from "./Audit";
import { SettingsPage } from "./Settings";
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
import { NotesContext, ToastProvider, Failure } from "./ui";
import brand from "./ds/assets/brand/ant-emblem.png";

export type Page =
  | "overview"
  | "packages"
  | "feeds"
  | "sources"
  | "identity"
  | "webhooks"
  | "audit"
  | "settings";
export type Go = (
  page: Page,
  options?: { feed?: string; create?: boolean; source?: string },
) => void;
const NAV: { id: Page; label: string; icon: string }[] = [
  { id: "overview", label: "Overview", icon: "gauge" },
  { id: "packages", label: "Packages", icon: "package" },
  { id: "feeds", label: "Feeds", icon: "rss" },
  { id: "sources", label: "Sources", icon: "database" },
  { id: "identity", label: "Access", icon: "shield" },
  { id: "webhooks", label: "Webhooks", icon: "webhook" },
  { id: "audit", label: "Audit log", icon: "scroll-text" },
  { id: "settings", label: "Settings", icon: "settings" },
];
type Theme = "light" | "dark";
function initialTheme(): Theme {
  try {
    const saved = localStorage.getItem("ant-theme");
    if (saved === "light" || saved === "dark") return saved;
  } catch {
    /* storage unavailable */
  }
  return typeof matchMedia === "function" &&
    matchMedia("(prefers-color-scheme: light)").matches
    ? "light"
    : "dark";
}

function Shell({
  client,
  sessionLabel,
  signOut,
  signingOut,
}: {
  client: Client;
  sessionLabel: string;
  signOut: () => void;
  signingOut: boolean;
}) {
  const [page, setPage] = useState<Page>("overview");
  const [feed, setFeed] = useState("");
  const [crumb, setCrumb] = useState("");
  const [createFeed, setCreateFeed] = useState(false);
  const [openSource, setOpenSource] = useState("");
  const [pkg, setPkg] = useState<PackageRef>();
  const [palette, setPalette] = useState(false);
  const [menu, setMenu] = useState(false);
  const [notes, setNotes] = useState(false);
  const [theme, setTheme] = useState<Theme>(initialTheme);
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
  const label = NAV.find((n) => n.id === page)!.label;
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
    body = <SourcesPage client={client} openId={openSource} />;
  else if (page === "identity") body = <Identity client={client} />;
  else if (page === "webhooks") body = <Webhooks client={client} />;
  else if (page === "audit") body = <AuditPage client={client} />;
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
              items={NAV}
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
              user={{ name: "Signed in", role: sessionLabel }}
              onSignOut={() => {
                if (!signingOut) signOut();
              }}
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
                onClick={() => {
                  const next = theme === "dark" ? "light" : "dark";
                  setTheme(next);
                  try {
                    localStorage.setItem("ant-theme", next);
                  } catch {
                    /* storage unavailable */
                  }
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
