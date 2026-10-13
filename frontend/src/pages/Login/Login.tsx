import { type FormEvent, useState } from 'react';
import { Icon, Logo } from '@/components/Icon/Icon';
import { ThemeToggle } from '@/components/ThemeToggle/ThemeToggle';
import { type OAuthProvider, useAuth } from '@/features/auth/AuthProvider';
import { setRemember } from '@/lib/supabase';

/**
 * Sign in, sign up, forgot password and choose-a-new-password — one card, four modes.
 *
 * Phone: the card alone, centred over the 3D scene. Desktop: the same card beside a pitch.
 *
 * Social sign-in: each provider must be switched on in the Supabase dashboard (Authentication →
 * Providers) with a client id and secret. Until then `signInWithProvider` checks first and shows a
 * sentence explaining that, instead of sending the learner to a raw Supabase error page.
 */

type Mode = 'signin' | 'signup' | 'forgot';

const FEATURES = [
    {
        icon: 'book',
        title: 'Your course, cited',
        body: 'Lectures, slides and textbooks become one knowledge base — every answer opens the exact page or timestamp.',
    },
    {
        icon: 'target',
        title: 'Adaptive assessment',
        body: 'Quizzes follow your mastery, with verified answers and feedback that cites the source.',
    },
    {
        icon: 'calendar',
        title: 'A plan that adapts',
        body: 'Weak topics, forgetting curves and your exam date shape every study day.',
    },
] as const;

const COPY: Record<Mode, { title: string; subtitle: string; submit: string }> = {
    signin: { title: 'Welcome', subtitle: 'Login to continue your journey', submit: 'Login' },
    signup: {
        title: 'Join Adigam',
        subtitle: 'Create an account to start learning',
        submit: 'Sign up',
    },
    forgot: {
        title: 'Reset',
        subtitle: "We'll email you a link to set a new password",
        submit: 'Send link',
    },
};

export default function Login() {
    const { signIn, signUp, signInWithProvider, resetPassword, updatePassword, recovering } =
        useAuth();

    const [mode, setMode] = useState<Mode>('signin');
    const [name, setName] = useState('');
    const [email, setEmail] = useState('');
    const [password, setPassword] = useState('');
    const [showPassword, setShowPassword] = useState(false);
    const [remember, setRememberChoice] = useState(true);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [notice, setNotice] = useState<string | null>(null);

    function switchMode(next: Mode) {
        setMode(next);
        if (next !== 'signup') setName('');
        // Errors belong to the attempt that produced them.
        setError(null);
        setNotice(null);
    }

    async function run(action: () => Promise<void>) {
        if (busy) return;
        setError(null);
        setNotice(null);
        setBusy(true);
        try {
            await action();
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Something went wrong. Try again.');
        } finally {
            setBusy(false);
        }
    }

    function submit(event: FormEvent) {
        event.preventDefault();
        void run(async () => {
            if (recovering) {
                await updatePassword(password);
                return;
            }
            if (mode === 'forgot') {
                await resetPassword(email.trim());
                setNotice(`If ${email.trim()} has an account, a reset link is on its way.`);
                return;
            }
            setRemember(remember);
            if (mode === 'signup') {
                const { needsEmailConfirm } = await signUp(email.trim(), password, name);
                if (needsEmailConfirm) {
                    setNotice(
                        `Account created. Check ${email.trim()} for a confirmation link, then log in.`
                    );
                    setMode('signin');
                    setPassword('');
                }
                // Without confirmation a session arrives and the router moves on by itself.
            } else {
                await signIn(email.trim(), password);
            }
        });
    }

    function social(provider: OAuthProvider) {
        setRemember(remember);
        void run(() => signInWithProvider(provider));
    }

    const copy = recovering
        ? { title: 'New password', subtitle: 'Choose a password for your account', submit: 'Save' }
        : COPY[mode];

    const needsEmail = !recovering;
    const needsPassword = recovering || mode !== 'forgot';

    return (
        <div className="authpage">
            <ThemeToggle className="themetoggle--float" />
            <div className="authpage__inner">
                {/* ---------------------------------------------- pitch (desktop) */}
                <div className="authpitch">
                    <div className="row rise" style={{ gap: 12 }}>
                        <Logo size={34} />
                        <span className="brand" style={{ fontSize: '1.45rem' }}>
                            Adigam<span> AI</span>
                        </span>
                    </div>

                    <div className="rise" style={{ '--i': 1 } as React.CSSProperties}>
                        <h1 className="authpitch__title">
                            Study from your material.
                            <br />
                            <span className="authpitch__accent">Master it for real.</span>
                        </h1>
                        <p className="muted" style={{ maxWidth: '48ch', fontSize: '1.08rem' }}>
                            An AI study companion that turns lecture videos, slides and textbooks
                            into source-cited tutoring and adaptive assessments.
                        </p>
                    </div>

                    <div className="stack" style={{ gap: 18 }}>
                        {FEATURES.map((feature, i) => (
                            <div
                                key={feature.title}
                                className="row rise authpitch__feature"
                                style={{ '--i': i + 2 } as React.CSSProperties}
                            >
                                <span className="tile">
                                    <Icon name={feature.icon} />
                                </span>
                                <div style={{ minWidth: 0 }}>
                                    <strong>{feature.title}</strong>
                                    <div className="faint">{feature.body}</div>
                                </div>
                            </div>
                        ))}
                    </div>
                </div>

                {/* ---------------------------------------------- card */}
                <div className="authcard rise" style={{ '--i': 1 } as React.CSSProperties}>
                    <div className="auth__badge">
                        <Padlock />
                    </div>

                    <h1 className="auth__title">{copy.title}</h1>
                    <p className="auth__subtitle">{copy.subtitle}</p>

                    {error && (
                        <div className="banner banner--error" role="alert">
                            {error}
                        </div>
                    )}
                    {notice && (
                        <div className="banner" role="status">
                            {notice}
                        </div>
                    )}

                    <form className="stack" style={{ gap: 20 }} onSubmit={submit}>
                        {mode === 'signup' && !recovering && (
                            <div className="inputwrap">
                                <span className="inputwrap__icon">
                                    <Icon name="users" size={20} />
                                </span>
                                <input
                                    aria-label="Your name"
                                    type="text"
                                    autoComplete="name"
                                    placeholder="Your name"
                                    maxLength={60}
                                    value={name}
                                    disabled={busy}
                                    required
                                    onChange={(e) => setName(e.target.value)}
                                />
                            </div>
                        )}

                        {needsEmail && (
                            <div className="inputwrap">
                                <span className="inputwrap__icon">
                                    <Icon name="mail" size={20} />
                                </span>
                                <input
                                    aria-label="Email address"
                                    type="email"
                                    autoComplete="email"
                                    placeholder="Email Address"
                                    value={email}
                                    disabled={busy}
                                    required
                                    onChange={(e) => setEmail(e.target.value)}
                                />
                            </div>
                        )}

                        {needsPassword && (
                            <div className="inputwrap">
                                <span className="inputwrap__icon">
                                    <Icon name="lock" size={20} />
                                </span>
                                <input
                                    aria-label="Password"
                                    type={showPassword ? 'text' : 'password'}
                                    autoComplete={
                                        mode === 'signin' && !recovering
                                            ? 'current-password'
                                            : 'new-password'
                                    }
                                    placeholder="Password"
                                    value={password}
                                    disabled={busy}
                                    required
                                    minLength={6}
                                    onChange={(e) => setPassword(e.target.value)}
                                />
                                <button
                                    type="button"
                                    className="inputwrap__action"
                                    aria-label={showPassword ? 'Hide password' : 'Show password'}
                                    onClick={() => setShowPassword((v) => !v)}
                                >
                                    <Icon name={showPassword ? 'eyeOff' : 'eye'} size={20} />
                                </button>
                            </div>
                        )}

                        {mode === 'signin' && !recovering && (
                            <div className="auth__row">
                                <label className="auth__check">
                                    <input
                                        type="checkbox"
                                        checked={remember}
                                        onChange={(e) => setRememberChoice(e.target.checked)}
                                    />
                                    Remember me
                                </label>
                                <button
                                    type="button"
                                    className="linkish"
                                    onClick={() => switchMode('forgot')}
                                >
                                    Forgot Password?
                                </button>
                            </div>
                        )}

                        <button type="submit" className="primary wide auth__submit" disabled={busy}>
                            {busy ? 'Please wait…' : copy.submit}
                            {!busy && <Arrow />}
                        </button>
                    </form>

                    {!recovering && (
                        <div className="auth__switch">
                            {mode === 'signin' ? (
                                <>
                                    <span>Don&rsquo;t have an account?</span>
                                    <button
                                        type="button"
                                        className="auth__round"
                                        aria-label="Create an account"
                                        onClick={() => switchMode('signup')}
                                    >
                                        <Plus />
                                    </button>
                                </>
                            ) : (
                                <>
                                    <span>
                                        {mode === 'signup'
                                            ? 'Already have an account?'
                                            : 'Remembered it?'}
                                    </span>
                                    <button
                                        type="button"
                                        className="auth__round"
                                        aria-label="Back to login"
                                        onClick={() => switchMode('signin')}
                                    >
                                        <Arrow back />
                                    </button>
                                </>
                            )}
                        </div>
                    )}

                    {mode !== 'forgot' && !recovering && (
                        <div className="auth__socials">
                            <button
                                type="button"
                                className="auth__social"
                                aria-label="Continue with Google"
                                disabled={busy}
                                onClick={() => social('google')}
                            >
                                <GoogleMark />
                            </button>
                            <button
                                type="button"
                                className="auth__social"
                                aria-label="Continue with Facebook"
                                disabled={busy}
                                onClick={() => social('facebook')}
                            >
                                <FacebookMark />
                            </button>
                            <button
                                type="button"
                                className="auth__social"
                                aria-label="Continue with LinkedIn"
                                disabled={busy}
                                onClick={() => social('linkedin_oidc')}
                            >
                                <LinkedInMark />
                            </button>
                        </div>
                    )}
                </div>
            </div>
        </div>
    );
}

/* ------------------------------------------------------------------ artwork */

/** A glossy gold padlock with its key — the badge at the top of the card. */
function Padlock() {
    return (
        <svg width="62" height="62" viewBox="0 0 64 64" aria-hidden="true">
            <defs>
                <linearGradient id="pl-gold" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0" stopColor="#ffe9a3" />
                    <stop offset="0.45" stopColor="#f5c443" />
                    <stop offset="1" stopColor="#c98a12" />
                </linearGradient>
                <linearGradient id="pl-steel" x1="0" y1="0" x2="1" y2="1">
                    <stop offset="0" stopColor="#fbfcff" />
                    <stop offset="0.5" stopColor="#aab3c6" />
                    <stop offset="1" stopColor="#6c7690" />
                </linearGradient>
            </defs>
            <path
                d="M20 30V21a12 12 0 0124 0v9"
                fill="none"
                stroke="url(#pl-steel)"
                strokeWidth="6"
                strokeLinecap="round"
            />
            <rect x="11" y="28" width="42" height="31" rx="8" fill="url(#pl-gold)" />
            <rect x="14" y="30.5" width="36" height="6" rx="3" fill="#fff" opacity="0.35" />
            <circle cx="32" cy="42" r="4.2" fill="#6b4708" />
            <rect x="30.4" y="44" width="3.2" height="8" rx="1.6" fill="#6b4708" />
            <path d="M46 20l-8 8" stroke="url(#pl-gold)" strokeWidth="3.4" strokeLinecap="round" />
            <path d="M41 25l-2.4-2.4" stroke="#c98a12" strokeWidth="2.4" strokeLinecap="round" />
            <circle cx="50" cy="15" r="8" fill="url(#pl-gold)" />
            <circle cx="50" cy="15" r="2.8" fill="#fff6d8" />
        </svg>
    );
}

function Arrow({ back = false }: { back?: boolean }) {
    return (
        <svg
            width="22"
            height="22"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.4"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
            style={back ? { transform: 'scaleX(-1)' } : undefined}
        >
            <path d="M4 12h15M13 6l6 6-6 6" />
        </svg>
    );
}

function Plus() {
    return (
        <svg
            width="26"
            height="26"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.8"
            strokeLinecap="round"
            aria-hidden="true"
        >
            <path d="M12 5v14M5 12h14" />
        </svg>
    );
}

function GoogleMark() {
    return (
        <svg width="26" height="26" viewBox="0 0 48 48" aria-hidden="true">
            <path
                fill="#EA4335"
                d="M24 9.5c3.5 0 6.6 1.2 9 3.6l6.7-6.7C35.6 2.4 30.2 0 24 0 14.6 0 6.6 5.4 2.7 13.3l7.8 6.1C12.4 13.6 17.7 9.5 24 9.5z"
            />
            <path
                fill="#4285F4"
                d="M46.1 24.6c0-1.6-.1-3.1-.4-4.6H24v8.7h12.4c-.5 2.8-2.1 5.2-4.5 6.8l7.3 5.7c4.3-4 6.9-9.9 6.9-16.6z"
            />
            <path
                fill="#FBBC05"
                d="M10.5 28.6c-.5-1.4-.8-3-.8-4.6s.3-3.2.8-4.6l-7.8-6.1C1 16.6 0 20.2 0 24s1 7.4 2.7 10.7l7.8-6.1z"
            />
            <path
                fill="#34A853"
                d="M24 48c6.5 0 11.9-2.1 15.9-5.8l-7.3-5.7c-2 1.4-4.7 2.3-8.6 2.3-6.3 0-11.6-4.1-13.5-9.9l-7.8 6.1C6.6 42.6 14.6 48 24 48z"
            />
        </svg>
    );
}

function FacebookMark() {
    return (
        <svg width="26" height="26" viewBox="0 0 24 24" aria-hidden="true">
            <path
                fill="#1877F2"
                d="M14 8.5V6.8c0-.8.5-1 .9-1h2.4V2h-3.3C10.3 2 9.6 4.8 9.6 6.6v1.9H7.5v3.9h2.1V22H14v-9.6h2.9l.4-3.9H14z"
            />
        </svg>
    );
}

function LinkedInMark() {
    return (
        <svg width="26" height="26" viewBox="0 0 24 24" aria-hidden="true">
            <path
                fill="#0A66C2"
                d="M4.98 3.5a2.5 2.5 0 110 5 2.5 2.5 0 010-5zM3 9.5h4V21H3zM9.5 9.5h3.8v1.6h.1c.5-1 1.8-2 3.8-2 4 0 4.8 2.6 4.8 6V21h-4v-5.2c0-1.2 0-2.8-1.7-2.8s-2 1.3-2 2.7V21h-4z"
            />
        </svg>
    );
}
