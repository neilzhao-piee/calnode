import { writable, get } from 'svelte/store';
import type { User } from './api';
import { localeTag, weekdayName } from './i18n';

export interface UserPrefs {
	timezone: string;
	time_format: '12h' | '24h';
	week_start: number;
	date_format: 'dmy' | 'mdy' | 'ymd';
}

const defaults: UserPrefs = {
	timezone: 'UTC',
	time_format: '12h',
	week_start: 1,
	date_format: 'dmy'
};

export const prefs = writable<UserPrefs>(defaults);

export function prefsFromUser(u: User): UserPrefs {
	return {
		timezone: u.timezone,
		time_format: u.time_format ?? '12h',
		week_start: u.week_start ?? 1,
		date_format: u.date_format ?? 'dmy'
	};
}

function fmtDatePart(date: Date, format: 'dmy' | 'mdy' | 'ymd'): string {
	const d = String(date.getDate()).padStart(2, '0');
	const m = String(date.getMonth() + 1).padStart(2, '0');
	const y = date.getFullYear();
	if (format === 'mdy') return `${m}/${d}/${y}`;
	if (format === 'ymd') return `${y}-${m}-${d}`;
	return `${d}/${m}/${y}`;
}

export function fmtDateTime(iso: string, p: UserPrefs = get(prefs)): string {
	const date = new Date(iso);
	const datePart = fmtDatePart(date, p.date_format);
	const timePart = date.toLocaleTimeString(localeTag(), {
		hour: '2-digit',
		minute: '2-digit',
		hour12: p.time_format === '12h'
	});
	return `${datePart}, ${timePart}`;
}

export function fmtDate(ymd: string, p: UserPrefs = get(prefs)): string {
	// ymd is always YYYY-MM-DD from the API — parse directly to avoid TZ shift from new Date()
	const [y, m, d] = ymd.split('-');
	if (p.date_format === 'mdy') return `${m}/${d}/${y}`;
	if (p.date_format === 'ymd') return `${y}-${m}-${d}`;
	return `${d}/${m}/${y}`;
}

export function fmtTime(iso: string, p: UserPrefs = get(prefs)): string {
	return new Date(iso).toLocaleTimeString(localeTag(), {
		hour: '2-digit',
		minute: '2-digit',
		hour12: p.time_format === '12h'
	});
}

/** Localized weekday names (Sunday = 0). Pass `$t` from a component so they update with the locale. */
export function weekDays(translateFn?: (key: string) => string): string[] {
	return [0, 1, 2, 3, 4, 5, 6].map((i) => weekdayName(i, translateFn));
}

// Curated quick-picks — kept for ordering and as the fallback when a browser
// lacks Intl.supportedValuesOf. Australia/Perth was added after Piee reported
// (2026-11-18) that the previous hand-picked 15 omitted it: the user could not
// save their own timezone from the UI even though the API accepts it.
const CURATED = [
	'Pacific/Auckland',
	'Australia/Sydney',
	'Australia/Melbourne',
	'Australia/Brisbane',
	'Australia/Perth',
	'Asia/Tokyo',
	'Asia/Singapore',
	'Asia/Dubai',
	'Europe/London',
	'Europe/Paris',
	'Europe/Berlin',
	'Europe/Amsterdam',
	'America/New_York',
	'America/Chicago',
	'America/Denver',
	'America/Los_Angeles',
	'UTC'
];

/**
 * Every IANA zone the browser knows (Baseline widely available since 2023),
 * curated entries first, deduped. The profile picker is a searchable Combobox,
 * so the full ~420-entry list is usable; the backend validates with
 * time.LoadLocation and accepts any IANA name. Falls back to CURATED only on
 * browsers without Intl.supportedValuesOf.
 */
function allTimezones(): string[] {
	let full: string[] = [];
	try {
		full = Intl.supportedValuesOf('timeZone');
	} catch {
		full = []; // older browser or unsupported key — curated list only
	}
	return [...new Set([...CURATED, ...full])];
}

export const TIMEZONES = allTimezones();
