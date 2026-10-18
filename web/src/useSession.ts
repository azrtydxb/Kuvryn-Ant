import { useEffect, useRef, useState } from "react";
import { Client } from "./api";
import { getSession, logout, type BrowserSession } from "./auth";
type Active = { client: Client; browser?: BrowserSession };
export function useSession() {
  const [active, setActive] = useState<Active>();
  const [checking, setChecking] = useState(true);
  const [error, setError] = useState<unknown>();
  const [logoutFailed, setLogoutFailed] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const [retry, setRetry] = useState(0);
  const generation = useRef(0);
  useEffect(() => {
    const n = ++generation.current;
    const controller = new AbortController();
    setChecking(true);
    setError(undefined);
    getSession(controller.signal)
      .then((s) => {
        if (generation.current === n && s)
          setActive({ client: Client.browser(s.csrf_token), browser: s });
      })
      .catch((e) => {
        if (generation.current === n) setError(e);
      })
      .finally(() => {
        if (generation.current === n) setChecking(false);
      });
    return () => {
      generation.current++;
      controller.abort();
    };
  }, [retry]);
  useEffect(() => {
    if (!active) return;
    const clear = () => setActive(undefined);
    const expiry = active.browser
      ? Date.parse(active.browser.session.ExpiresAt) - Date.now()
      : 15 * 60 * 1000;
    const timer = setTimeout(clear, Math.max(0, Math.min(expiry, 2147483647)));
    window.addEventListener("pagehide", clear);
    return () => {
      clearTimeout(timer);
      window.removeEventListener("pagehide", clear);
      active.client.dispose();
    };
  }, [active]);
  function accept(browser: BrowserSession) {
    generation.current++;
    setChecking(false);
    setError(undefined);
    setLogoutFailed(false);
    setActive({ client: Client.browser(browser.csrf_token), browser });
  }
  function useToken(token: string) {
    generation.current++;
    setChecking(false);
    setError(undefined);
    setActive({ client: new Client(token) });
  }
  async function signOut() {
    const browser = active?.browser;
    active?.client.dispose();
    setActive(undefined);
    setError(undefined);
    setLoggingOut(true);
    try {
      if (browser) await logout(browser.csrf_token);
      else if (logoutFailed) {
        const session = await getSession();
        if (session) await logout(session.csrf_token);
      }
      setLogoutFailed(false);
    } catch (e) {
      setError(e);
      setLogoutFailed(true);
    } finally {
      setLoggingOut(false);
    }
  }
  return {
    client: active?.client,
    browser: active?.browser,
    checking,
    error,
    logoutFailed,
    loggingOut,
    accept,
    useToken,
    signOut,
    recheck: () => setRetry((n) => n + 1),
  };
}
