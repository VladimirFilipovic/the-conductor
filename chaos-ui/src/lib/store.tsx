"use client";

import {
  createContext,
  useContext,
  useCallback,
  useEffect,
  useState,
} from "react";

export interface Meta {
  projects: { name: string }[];
  environments: { id: string; project_name: string; name: string }[];
  regions: string[];
}

export interface ChaosEntry {
  id: string;
  ts: string;
  session: string;
  action: string;
  detail: string;
  ok: boolean;
}

interface Store {
  session: string;
  project: string;
  environment: string;
  region: string;
  setProject: (v: string) => void;
  setEnvironment: (v: string) => void;
  setRegion: (v: string) => void;
  meta: Meta;
  refreshMeta: () => void;
  chaosLog: ChaosEntry[];
  logChaos: (action: string, detail: string, ok: boolean) => void;
  clearChaos: () => void;
}

const Ctx = createContext<Store | null>(null);

const EMPTY_META: Meta = { projects: [], environments: [], regions: [] };

function makeSession(): string {
  return "sess_" + Math.random().toString(36).slice(2, 10);
}

function loadLS(key: string, fallback: string): string {
  if (typeof window === "undefined") return fallback;
  return window.localStorage.getItem(key) ?? fallback;
}

export function AppProvider({ children }: { children: React.ReactNode }) {
  const [session, setSession] = useState("");
  const [project, setProjectState] = useState("all");
  const [environment, setEnvironmentState] = useState("all");
  const [region, setRegionState] = useState("all");
  const [meta, setMeta] = useState<Meta>(EMPTY_META);
  const [chaosLog, setChaosLog] = useState<ChaosEntry[]>([]);

  useEffect(() => {
    let s = window.localStorage.getItem("chaos.session");
    if (!s) {
      s = makeSession();
      window.localStorage.setItem("chaos.session", s);
    }
    setSession(s);
    setProjectState(loadLS("chaos.project", "all"));
    setEnvironmentState(loadLS("chaos.environment", "all"));
    setRegionState(loadLS("chaos.region", "all"));
    try {
      setChaosLog(JSON.parse(window.localStorage.getItem("chaos.log") || "[]"));
    } catch {
      setChaosLog([]);
    }
  }, []);

  const refreshMeta = useCallback(() => {
    fetch("/api/meta")
      .then((r) => r.json())
      .then((m: Meta) => setMeta(m.projects ? m : EMPTY_META))
      .catch(() => {});
  }, []);

  useEffect(() => {
    refreshMeta();
    const t = setInterval(refreshMeta, 5000);
    return () => clearInterval(t);
  }, [refreshMeta]);

  const setProject = useCallback((v: string) => {
    setProjectState(v);
    window.localStorage.setItem("chaos.project", v);
    // Environment names are project-scoped; a stale one would silently filter
    // everything out, so reset it when the project changes.
    setEnvironmentState("all");
    window.localStorage.setItem("chaos.environment", "all");
  }, []);

  const setEnvironment = useCallback((v: string) => {
    setEnvironmentState(v);
    window.localStorage.setItem("chaos.environment", v);
  }, []);

  const setRegion = useCallback((v: string) => {
    setRegionState(v);
    window.localStorage.setItem("chaos.region", v);
  }, []);

  const logChaos = useCallback(
    (action: string, detail: string, ok: boolean) => {
      setChaosLog((prev) => {
        const entry: ChaosEntry = {
          id: Math.random().toString(36).slice(2),
          ts: new Date().toISOString(),
          session:
            window.localStorage.getItem("chaos.session") || "unknown",
          action,
          detail,
          ok,
        };
        const next = [entry, ...prev].slice(0, 200);
        window.localStorage.setItem("chaos.log", JSON.stringify(next));
        return next;
      });
    },
    [],
  );

  const clearChaos = useCallback(() => {
    setChaosLog([]);
    window.localStorage.setItem("chaos.log", "[]");
  }, []);

  return (
    <Ctx.Provider
      value={{
        session,
        project,
        environment,
        region,
        setProject,
        setEnvironment,
        setRegion,
        meta,
        refreshMeta,
        chaosLog,
        logChaos,
        clearChaos,
      }}
    >
      {children}
    </Ctx.Provider>
  );
}

export function useStore(): Store {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error("useStore outside AppProvider");
  return ctx;
}

// Build the topology query string from the active global selection.
export function selectionQuery(s: Store): string {
  const p = new URLSearchParams();
  if (s.project !== "all") p.set("project", s.project);
  if (s.environment !== "all") p.set("environment", s.environment);
  if (s.region !== "all") p.set("region", s.region);
  const q = p.toString();
  return q ? `?${q}` : "";
}
