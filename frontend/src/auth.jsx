import { createContext, useContext, useEffect, useState, useCallback } from "react";
import { api } from "./api.js";

const AuthCtx = createContext(null);
const KEY = "rmmway.operator.token";

export function AuthProvider({ children }) {
  const [token, setToken] = useState(() => localStorage.getItem(KEY) || null);
  const [booting, setBooting] = useState(false);
  const [error, setError] = useState(null);

  const login = useCallback(async (username, password) => {
    setBooting(true);
    setError(null);
    try {
      const { token } = await api.login(username, password);
      localStorage.setItem(KEY, token);
      setToken(token);
      return true;
    } catch (e) {
      setError(e.message || "login failed");
      return false;
    } finally {
      setBooting(false);
    }
  }, []);

  const logout = useCallback(() => {
    localStorage.removeItem(KEY);
    setToken(null);
  }, []);

  // On mount, if we restored a token, probe the API once so an expired or
  // invalid token (e.g. server restarted with a new RMMWAY_JWT_SECRET)
  // gets cleared instead of being presented as a working session.
  useEffect(() => {
    if (!token) return;
    let alive = true;
    api
      .devices(token)
      .catch(() => {
        if (!alive) return;
        localStorage.removeItem(KEY);
        setToken(null);
      });
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <AuthCtx.Provider value={{ token, booting, error, login, logout }}>
      {children}
    </AuthCtx.Provider>
  );
}

export function useAuth() {
  return useContext(AuthCtx);
}
