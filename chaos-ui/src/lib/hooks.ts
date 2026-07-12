"use client";

import { useEffect, useRef, useState } from "react";

// usePoll fetches `url` immediately then every `intervalMs`, exposing the latest
// parsed JSON plus a first-load flag. It skips overlapping requests and ignores
// responses that arrive after the url changed, so a fast selector switch never
// paints stale data.
export function usePoll<T>(url: string, intervalMs: number) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const inFlight = useRef(false);
  const currentUrl = useRef(url);

  useEffect(() => {
    currentUrl.current = url;
    let cancelled = false;

    const run = async () => {
      if (inFlight.current) return;
      inFlight.current = true;
      const forUrl = url;
      try {
        const res = await fetch(forUrl, { cache: "no-store" });
        const json = await res.json();
        if (!cancelled && currentUrl.current === forUrl) {
          if (res.ok) {
            setData(json as T);
            setError(null);
          } else {
            setError(json.error || `HTTP ${res.status}`);
          }
        }
      } catch (e) {
        if (!cancelled) setError(String(e));
      } finally {
        inFlight.current = false;
        if (!cancelled) setLoading(false);
      }
    };

    setLoading(true);
    run();
    const t = setInterval(run, intervalMs);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [url, intervalMs]);

  return { data, error, loading };
}
