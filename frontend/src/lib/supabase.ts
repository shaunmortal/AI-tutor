import { AuthClient } from '@supabase/auth-js';

/**
 * The browser's auth client.
 *
 * Deliberately `@supabase/auth-js` and not the full `@supabase/supabase-js`. All this app
 * needs from Supabase is sign-in, sign-up, sign-out and the session — every piece of data
 * comes from our own API. The full client additionally bundles postgrest, realtime
 * (websockets), storage and edge-function clients, none of which this app calls, and together
 * they roughly doubled the gzipped bundle. On a 2GB Android phone over a slow connection that
 * is not an acceptable price for code that never runs.
 *
 * Only the publishable key is here, which is fine: it is compiled into the bundle and meant to
 * be public. It grants nothing on its own — every table is behind Row Level Security, which
 * needs a signed-in learner's token. The secret key never appears in the frontend at all; it
 * lives in the backend's environment.
 */

const url = import.meta.env.VITE_SUPABASE_URL;
const publishableKey = import.meta.env.VITE_SUPABASE_PUBLISHABLE_KEY;

if (!url || !publishableKey) {
    // Fail loudly at startup rather than with a confusing error on the first request.
    throw new Error(
        'Missing VITE_SUPABASE_URL or VITE_SUPABASE_PUBLISHABLE_KEY. Copy frontend/.env.example to frontend/.env.'
    );
}

/**
 * "Remember me". Ticked, the session lives in localStorage and survives closing the browser;
 * unticked, it lives in sessionStorage and ends with the tab — the right default on a shared
 * college computer. Reads check both, so a session written either way is found on reload.
 */
const REMEMBER_KEY = 'adigam-remember';

export function setRemember(remember: boolean): void {
    try {
        localStorage.setItem(REMEMBER_KEY, remember ? '1' : '0');
    } catch {
        // Storage blocked: the session simply lasts as long as the page.
    }
}

function remembered(): boolean {
    try {
        return localStorage.getItem(REMEMBER_KEY) !== '0';
    } catch {
        return false;
    }
}

const storage = {
    getItem(key: string) {
        try {
            return localStorage.getItem(key) ?? sessionStorage.getItem(key);
        } catch {
            return null;
        }
    },
    setItem(key: string, value: string) {
        try {
            (remembered() ? localStorage : sessionStorage).setItem(key, value);
        } catch {
            // Private mode: nothing persists, the in-memory session still works.
        }
    },
    removeItem(key: string) {
        try {
            localStorage.removeItem(key);
            sessionStorage.removeItem(key);
        } catch {
            // Nothing stored, nothing to remove.
        }
    },
};

const auth = new AuthClient({
    storage,
    // The paths supabase-js would have built for us.
    url: `${url}/auth/v1`,
    headers: { apikey: publishableKey, Authorization: `Bearer ${publishableKey}` },
    // Persist (where "Remember me" says) and refresh in the background, as the full client does.
    persistSession: true,
    autoRefreshToken: true,
    detectSessionInUrl: true,
});

/**
 * Shaped like `supabase.auth` so call sites read the same as they would with the full client,
 * and so swapping back is a one-file change if the browser ever needs direct table reads.
 */
export const supabase = { auth };
