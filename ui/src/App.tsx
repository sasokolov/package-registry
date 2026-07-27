import { useCallback, useState } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { useResource } from "./api/hooks";
import type { SiteStatus, WhoAmI } from "./api/types";
import { Layout } from "./components/Layout";
import { SignIn } from "./components/SignIn";
import { Configuration } from "./pages/Configuration";
import { Conflicts } from "./pages/Conflicts";
import { FeedDetail } from "./pages/FeedDetail";
import { Feeds } from "./pages/Feeds";
import { Overview } from "./pages/Overview";
import { Quarantine } from "./pages/Quarantine";
import { Replication } from "./pages/Replication";
import { Tokens } from "./pages/Tokens";

export function App() {
  // Bumping this re-reads who we are, which is what signing in and out
  // actually change.
  const [session, setSession] = useState(0);
  const refresh = useCallback(() => setSession((n) => n + 1), []);

  const who = useResource<WhoAmI>("/whoami", [session]);
  const status = useResource<SiteStatus>("/status", [session]);

  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Layout status={status.data} who={who.data} onSignOut={refresh} />}>
          <Route path="/ui/" element={<Overview />} />
          <Route path="/ui/feeds" element={<Feeds />} />
          <Route path="/ui/feeds/:feed" element={<FeedDetail />} />
          <Route path="/ui/replication" element={<Replication />} />
          <Route path="/ui/conflicts" element={<Conflicts who={who.data} />} />
          <Route path="/ui/quarantine" element={<Quarantine who={who.data} />} />
          <Route path="/ui/tokens" element={<Tokens />} />
          <Route path="/ui/config" element={<Configuration />} />
          <Route path="/ui/signin" element={<SignIn onSignedIn={refresh} />} />
          <Route path="*" element={<Navigate to="/ui/" replace />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
