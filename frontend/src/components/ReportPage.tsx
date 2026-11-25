
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

type AuthStatus =
  | 'checking'
  | 'authenticated'
  | 'unauthenticated'
  | 'forbidden';

type ReportDaysResponse = {
  days: string[];
};

const AUTH_URL = (process.env.REACT_APP_AUTH_API_URL || '').replace(/\/+$/, '');
const REPORT_API_URL = (process.env.REACT_APP_REPORT_API_URL || '').replace(/\/+$/, '');

function authEndpoint(path: string): string {
  return `${AUTH_URL}${path}`;
}

function reportEndpoint(path: string): string {
  return `${REPORT_API_URL}${path}`;
}

// React.StrictMode may re-run effects in development.
// Reuse the initial session check.
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

      const body = await response.text();
      return body ? JSON.parse(body) as SessionInfo : {};
    }).catch((error) => {
      sessionCheckPromise = null;
      throw error;
    });
  }

  return sessionCheckPromise;
}

function getLastSevenDays(): string[] {
  const result: string[] = [];
  const today = new Date();

  for (let i = 0; i < 7; i++) {
    const date = new Date(today);
    date.setDate(today.getDate() - i);

    const year = date.getFullYear();
    const month = String(date.getMonth() + 1).padStart(2, '0');
    const day = String(date.getDate()).padStart(2, '0');

    result.push(`${year}-${month}-${day}`);
  }

  return result;
}

function formatDate(date: string): string {
  const [year, month, day] = date.split('-');
  return `${day}.${month}.${year}`;
}

const ReportPage: React.FC = () => {
  const [authStatus, setAuthStatus] = useState<AuthStatus>('checking');
  const [session, setSession] = useState<SessionInfo | null>(null);

  const [reportDays, setReportDays] = useState<string[]>([]);
  const [loadingDays, setLoadingDays] = useState(false);
  const [downloadingDate, setDownloadingDate] = useState<string | null>(null);

  const [error, setError] = useState<string | null>(null);

  // Session is invalid or expired.
  const handleUnauthorized = () => {
    setAuthStatus('unauthenticated');
    setSession(null);
    setReportDays([]);
    setError('Сессия истекла. Войдите снова.');

    sessionCheckPromise = null;
  };

  // Session is valid, but the user has insufficient permissions.
  const handleForbidden = () => {
    setAuthStatus('forbidden');
    setReportDays([]);
    setError(null);

    // Do not clear the session.
    // The user is authenticated, but lacks prosthetic_user role.
  };

  const loadReportDays = async () => {
    try {
      setLoadingDays(true);
      setError(null);

      const response = await fetch(reportEndpoint('/report/days'), {
        method: 'GET',
        credentials: 'include',
        headers: {
          Accept: 'application/json',
        },
      });

      if (response.status === 401) {
        handleUnauthorized();
        return;
      }

      if (response.status === 403) {
        handleForbidden();
        return;
      }

      if (!response.ok) {
        throw new Error(`Failed to load report dates (${response.status})`);
      }

      const data = await response.json() as ReportDaysResponse;

      const lastSevenDays = new Set(getLastSevenDays());

      const availableDays = (data.days || [])
        .filter((date) => lastSevenDays.has(date))
        .sort((a, b) => b.localeCompare(a));

      setReportDays(availableDays);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : 'Unable to load reports'
      );
    } finally {
      setLoadingDays(false);
    }
  };

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

        const message = err instanceof Error
          ? err.message
          : 'Unable to check session';

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

  useEffect(() => {
    if (authStatus === 'authenticated') {
      loadReportDays();
    }
  }, [authStatus]);

  const login = () => {
    window.location.assign(authEndpoint('/auth/login'));
  };

  const downloadReport = async (date: string) => {
    try {
      setDownloadingDate(date);
      setError(null);

      const response = await fetch(
        reportEndpoint(`/report?date=${encodeURIComponent(date)}`),
        {
          method: 'GET',
          credentials: 'include',
          headers: {
            Accept: 'text/csv, */*',
          },
        }
      );

      if (response.status === 401) {
        handleUnauthorized();
        return;
      }

      if (response.status === 403) {
        handleForbidden();
        return;
      }

      if (!response.ok) {
        throw new Error(`Report request failed (${response.status})`);
      }

      const blob = await response.blob();
      const objectUrl = window.URL.createObjectURL(blob);

      const link = document.createElement('a');

      const disposition = response.headers.get('Content-Disposition') || '';

      const filenameMatch = disposition.match(
        /filename\*?=(?:UTF-8''|")?([^";]+)/i
      );

      const filename = filenameMatch?.[1]
        ? decodeURIComponent(filenameMatch[1].replace(/"/g, ''))
        : `report-${date}.csv`;

      link.href = objectUrl;
      link.download = filename;

      document.body.appendChild(link);
      link.click();
      link.remove();

      window.URL.revokeObjectURL(objectUrl);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : 'Unable to download report'
      );
    } finally {
      setDownloadingDate(null);
    }
  };

  const displayName =
    session?.user?.name ||
    session?.user?.preferred_username ||
    session?.username ||
    session?.user?.email ||
    session?.email;

  // Session check in progress.
  if (authStatus === 'checking') {
    return (
      <div className="flex items-center justify-center min-h-screen bg-gray-100">
        Checking session...
      </div>
    );
  }

  // User is not authenticated.
  if (authStatus === 'unauthenticated') {
    return (
      <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
        <div className="p-8 bg-white rounded-lg shadow-md">
          <h1 className="text-2xl font-bold mb-6">
            Usage Reports
          </h1>

          {error && (
            <div className="mb-4 p-4 bg-red-100 text-red-700 rounded">
              {error}
            </div>
          )}

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

  // User is authenticated, but lacks prosthetic_user role.
  if (authStatus === 'forbidden') {
    return (
      <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
        <div className="p-8 bg-white rounded-lg shadow-md max-w-md w-full">
          <div className="flex items-center gap-3 mb-4">
            <div className="flex items-center justify-center w-10 h-10 bg-red-100 rounded-full">
              <span className="text-red-600 text-xl font-bold">!</span>
            </div>

            <h1 className="text-2xl font-bold">
              Access Denied
            </h1>
          </div>

          {displayName && (
            <p className="text-gray-600 mb-4">
              Signed in as {displayName}
            </p>
          )}

          <div className="p-4 bg-red-50 text-red-700 rounded">
            У вас нет прав для просмотра отчётов.
            Необходима роль prosthetic_user.
          </div>
        </div>
      </div>
    );
  }

  // Authenticated user with sufficient permissions.
  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
      <div className="w-full max-w-lg p-8 bg-white rounded-lg shadow-md">
        <h1 className="text-2xl font-bold mb-2">
          Usage Reports
        </h1>

        {displayName && (
          <p className="text-gray-600 mb-6">
            Signed in as {displayName}
          </p>
        )}

        <h2 className="text-lg font-semibold mb-3">
          Available reports (last 7 days)
        </h2>

        {loadingDays && (
          <p className="text-gray-500 py-4">
            Loading reports...
          </p>
        )}

        {!loadingDays && reportDays.length === 0 && (
          <p className="text-gray-500 py-4">
            No reports available for the last 7 days.
          </p>
        )}

        {reportDays.length > 0 && (
          <div className="divide-y divide-gray-200 border-t border-b">
            {reportDays.map((date) => (
              <div
                key={date}
                className="flex items-center justify-between py-4"
              >
                <div>
                  <span className="font-medium text-gray-800">
                    {formatDate(date)}
                  </span>

                  {date === getLastSevenDays()[0] && (
                    <span className="ml-2 text-xs text-gray-500">
                      Today
                    </span>
                  )}
                </div>

                <button
                  onClick={() => downloadReport(date)}
                  disabled={downloadingDate !== null}
                  className={`px-4 py-2 text-sm bg-blue-500 text-white rounded hover:bg-blue-600 ${
                    downloadingDate !== null
                      ? 'opacity-50 cursor-not-allowed'
                      : ''
                  }`}
                >
                  {downloadingDate === date
                    ? 'Downloading...'
                    : 'Download CSV'}
                </button>
              </div>
            ))}
          </div>
        )}

        <button
          onClick={loadReportDays}
          disabled={loadingDays}
          className="mt-4 text-sm text-blue-600 hover:text-blue-800 disabled:opacity-50"
        >
          Refresh list
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

