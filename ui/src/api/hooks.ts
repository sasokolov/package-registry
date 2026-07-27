import { useCallback, useEffect, useState } from "react";
import { ApiError, api } from "./client";

export interface Resource<T> {
  data: T | undefined;
  etag: string | undefined;
  error: ApiError | undefined;
  loading: boolean;
  reload: () => void;
}

/**
 * Fetches a resource and re-fetches on demand. Deliberately small: the
 * console reads a handful of endpoints, and a caching layer would be a
 * second source of truth about what the registry currently says.
 */
export function useResource<T>(path: string | undefined, deps: unknown[] = []): Resource<T> {
  const [data, setData] = useState<T>();
  const [etag, setEtag] = useState<string>();
  const [error, setError] = useState<ApiError>();
  const [loading, setLoading] = useState(Boolean(path));
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (!path) {
      setLoading(false);
      return;
    }
    const controller = new AbortController();
    setLoading(true);
    api
      .get<T>(path, controller.signal)
      .then((envelope) => {
        setData(envelope.data);
        setEtag(envelope.etag);
        setError(undefined);
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setError(err instanceof ApiError ? err : new ApiError(0, String(err)));
        setData(undefined);
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, nonce, ...deps]);

  return { data, etag, error, loading, reload };
}

/**
 * Polls a resource on an interval — used for the live status views. A
 * undefined path means "there is nothing to ask for", which is how a screen
 * skips an endpoint the current identity may not read.
 */
export function usePolledResource<T>(
  path: string | undefined,
  intervalMs: number,
  deps: unknown[] = [],
): Resource<T> {
  const resource = useResource<T>(path, deps);
  const { reload } = resource;
  useEffect(() => {
    if (!path) return;
    const timer = setInterval(reload, intervalMs);
    return () => clearInterval(timer);
  }, [path, reload, intervalMs]);
  return resource;
}
