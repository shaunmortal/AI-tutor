import { useTheme } from '@/hooks/useTheme';

/** Day ↔ night in one tap. "Match device" stays in Settings for people who want it. */
export function ThemeToggle({ className = '' }: { className?: string }) {
    const { resolved, setTheme } = useTheme();
    const night = resolved === 'dark';

    return (
        <button
            type="button"
            className={`themetoggle ${className}`}
            aria-label={night ? 'Switch to day mode' : 'Switch to night mode'}
            title={night ? 'Day mode' : 'Night mode'}
            onClick={() => setTheme(night ? 'light' : 'dark')}
        >
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                {night ? (
                    <>
                        <circle cx="12" cy="12" r="4.5" />
                        <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
                    </>
                ) : (
                    <path d="M20.5 14.5A8.5 8.5 0 019.5 3.5a8.5 8.5 0 1011 11z" />
                )}
            </svg>
        </button>
    );
}
