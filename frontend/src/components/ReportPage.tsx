import React, { useEffect, useState } from 'react';

type SessionInfo = {
  authenticated?: boolean;
  user?: {
    name?: string;
    email?: string;
    preferred_username?: string;
  };
  username?: string;
  email?: string;
};

type AuthStatus = 'checking' | 'authenticated' | 'unauthenticated';

const AUTH_URL = (process.env.REACT_APP_AUTH_API_URL || '').replace(/\/+$/, '');
const REPORT_API_URL = (process.env.REACT_APP_REPORT_API_URL || '').replace(/\/+$/, '');

function authEndpoint(path: string): string {
  return `${AUTH_URL}${path}`;
}

function reportEndpoint(path: string): string {
  return `${REPORT_API_URL}${path}`;
}

// React.StrictMode intentionally re-runs effects in development. Reuse the
// initial session check so the page does not issue duplicate auth requests.
// Session ID rotation happens inside reports-api's protected-request flow.
let sessionCheckPromise: Promise<SessionInfo> | null = null;

function checkSession(): Promise<SessionInfo> {
  if (!sessionCheckPromise) {
    sessionCheckPromise = fetch(authEndpoint('/auth/session'), {
      method: 'GET',
      credentials: 'include',
      headers: { Accept: 'application/json' },
    }).then(async (response) => {
      if (response.status === 401 || response.status === 403) {
        throw new Error('Not authenticated');
      }
      if (!response.ok) {
        throw new Error(`Session check failed (${response.status})`);
      }

      // The backend may return an empty successful response in older builds.
      const body = await response.text();
      return body ? JSON.parse(body) as SessionInfo : {};
    }).catch((error) => {
      // Allow an explicit retry after a transient network/server failure.
      sessionCheckPromise = null;
      throw error;
    });
  }
  return sessionCheckPromise;
}

const ReportPage: React.FC = () => {
  const [authStatus, setAuthStatus] = useState<AuthStatus>('checking');
  const [session, setSession] = useState<SessionInfo | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let active = true;

    checkSession()
      .then((info) => {
        if (!active) return;
        setSession(info);
        setAuthStatus('authenticated');
      })
      .catch((err: unknown) => {
        if (!active) return;
        const message = err instanceof Error ? err.message : 'Unable to check session';
        if (message === 'Not authenticated') {
          setAuthStatus('unauthenticated');
        } else {
          setError(message);
          setAuthStatus('unauthenticated');
        }
      });

    return () => {
      active = false;
    };
  }, []);

  const login = () => {
    // The BFF starts Authorization Code + PKCE and handles the Keycloak callback.
    window.location.assign(authEndpoint('/auth/login'));
  };

  const downloadReport = async () => {
    try {
      setLoading(true);
      setError(null);

      const response = await fetch(reportEndpoint('/reports'), {
        method: 'GET',
        credentials: 'include',
        headers: { Accept: 'application/octet-stream, application/pdf, */*' },
      });

      if (response.status === 401 || response.status === 403) {
        setAuthStatus('unauthenticated');
        setSession(null);
        setError('Сессия истекла. Войдите снова.');
        return;
      }

      if (!response.ok) {
        throw new Error(`Report request failed (${response.status})`);
      }

      const blob = await response.blob();
      const objectUrl = window.URL.createObjectURL(blob);
      const link = document.createElement('a');
      const disposition = response.headers.get('Content-Disposition') || '';
      const filenameMatch = disposition.match(/filename\*?=(?:UTF-8''|")?([^";]+)/i);
      const filename = filenameMatch?.[1]
        ? decodeURIComponent(filenameMatch[1].replace(/"/g, ''))
        : 'report';

      link.href = objectUrl;
      link.download = filename;
      document.body.appendChild(link);
      link.click();
      link.remove();
      window.URL.revokeObjectURL(objectUrl);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'An error occurred');
    } finally {
      setLoading(false);
    }
  };

  if (authStatus === 'checking') {
    return <div className="flex items-center justify-center min-h-screen bg-gray-100">Checking session...</div>;
  }

  if (authStatus === 'unauthenticated') {
    return (
      <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
        <div className="p-8 bg-white rounded-lg shadow-md">
          <h1 className="text-2xl font-bold mb-6">Usage Reports</h1>
          {error && <div className="mb-4 p-4 bg-red-100 text-red-700 rounded">{error}</div>}
          <button
            onClick={login}
            className="px-4 py-2 bg-blue-500 text-white rounded hover:bg-blue-600"
          >
            Login
          </button>
        </div>
      </div>
    );
  }

  const displayName =
    session?.user?.name ||
    session?.user?.preferred_username ||
    session?.username ||
    session?.user?.email ||
    session?.email;

  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
      <div className="p-8 bg-white rounded-lg shadow-md">
        <h1 className="text-2xl font-bold mb-2">Usage Reports</h1>
        {displayName && <p className="text-gray-600 mb-6">Signed in as {displayName}</p>}
        {!displayName && <div className="mb-6" />}
        <button
          onClick={downloadReport}
          disabled={loading}
          className={`px-4 py-2 bg-blue-500 text-white rounded hover:bg-blue-600 ${
            loading ? 'opacity-50 cursor-not-allowed' : ''
          }`}
        >
          {loading ? 'Generating Report...' : 'Download Report'}
        </button>

        {error && (
          <div className="mt-4 p-4 bg-red-100 text-red-700 rounded">
            {error}
          </div>
        )}
      </div>
    </div>
  );
};

export default ReportPage;
