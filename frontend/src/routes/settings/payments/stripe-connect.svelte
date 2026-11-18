<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { api, type StripeConnectList, type StripeConnectAccount, type TeamMember } from '$lib/api';
	import { Button } from '$lib/components/ui/button';
	import { Switch } from '$lib/components/ui/switch';
	import { Badge } from '$lib/components/ui/badge';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import * as Select from '$lib/components/ui/select';
	import { toast } from 'svelte-sonner';
	import { t } from '$lib/i18n';

	let list = $state<StripeConnectList | null>(null);
	let members = $state<TeamMember[]>([]);
	let enabled = $state(false);
	let busy = $state(false);
	let unlinkOpen = $state(false);
	let unlinkTarget = $state<StripeConnectAccount | null>(null);
	let pickUser = $state('');

	// Hosts that can receive payouts but are not yet connected (active, non-archived).
	const candidates = $derived(
		members.filter((m) => !m.archived && !list?.accounts.some((a) => a.user_id === m.id))
	);

	async function load() {
		list = await api.get<StripeConnectList>('/v1/settings/stripe/connect/accounts');
		enabled = list.enabled;
	}

	onMount(async () => {
		try {
			await Promise.all([
				load(),
				api.get<TeamMember[]>('/v1/users').then((u) => (members = u))
			]);
			// Returned from Stripe-hosted onboarding: nudge the admin to refresh status.
			if ($page.url.searchParams.get('connect') === 'return') {
				toast.info($t('settings.payments.connect.toast.returned'));
			}
		} catch {
			toast.error($t('settings.payments.connect.error.load'));
		}
	});

	async function toggle(next: boolean) {
		const prev = enabled;
		enabled = next;
		busy = true;
		try {
			const res = await api.patch<{ connect_enabled: boolean }>('/v1/settings/stripe', {
				connect_enabled: next
			});
			enabled = res.connect_enabled;
			toast.success(enabled ? $t('settings.payments.connect.toast.modeOn') : $t('settings.payments.connect.toast.modeOff'));
		} catch {
			enabled = prev; // revert the switch
			toast.error($t('settings.payments.connect.error.save'));
		} finally {
			busy = false;
		}
	}

	async function addHost() {
		if (!pickUser) return;
		busy = true;
		try {
			await api.post('/v1/settings/stripe/connect/accounts', { user_id: pickUser });
			const link = await api.post<{ url: string }>('/v1/settings/stripe/connect/links', { user_id: pickUser });
			await load();
			pickUser = '';
			window.open(link.url, '_blank', 'noopener');
			toast.success($t('settings.payments.connect.toast.created'));
		} catch (e) {
			toast.error(e instanceof Error ? e.message : $t('settings.payments.connect.error.save'));
		} finally {
			busy = false;
		}
	}

	async function onboard(a: StripeConnectAccount) {
		busy = true;
		try {
			const link = await api.post<{ url: string }>('/v1/settings/stripe/connect/links', { user_id: a.user_id });
			window.open(link.url, '_blank', 'noopener');
		} catch (e) {
			toast.error(e instanceof Error ? e.message : $t('settings.payments.connect.error.save'));
		} finally {
			busy = false;
		}
	}

	async function refresh() {
		busy = true;
		try {
			await load();
		} catch {
			toast.error($t('settings.payments.connect.error.load'));
		} finally {
			busy = false;
		}
	}

	async function confirmUnlink() {
		if (!unlinkTarget) return;
		busy = true;
		try {
			await api.del(`/v1/settings/stripe/connect/accounts?user_id=${encodeURIComponent(unlinkTarget.user_id)}`);
			await load();
			toast.success($t('settings.payments.connect.toast.unlinked'));
		} catch {
			toast.error($t('settings.payments.connect.error.save'));
		} finally {
			unlinkTarget = null;
			busy = false;
		}
	}

	// ready = money can both arrive and be paid out; error = Stripe unreachable.
	function statusOf(a: StripeConnectAccount): 'ready' | 'onboarding' | 'error' {
		if (a.status_error) return 'error';
		return a.charges_enabled && a.payouts_enabled && a.details_submitted ? 'ready' : 'onboarding';
	}
</script>

<div class="rounded-lg border bg-card p-6">
	<div class="mb-1 flex items-start justify-between gap-4">
		<div>
			<h2 class="text-sm font-semibold">{$t('settings.payments.connect.title')}</h2>
			<p class="mt-0.5 text-xs text-muted-foreground">{$t('settings.payments.connect.subtitle')}</p>
		</div>
		<Switch bind:checked={enabled} disabled={busy} onCheckedChange={toggle} aria-label={$t('settings.payments.connect.enable')} />
	</div>
	<p class="text-xs text-muted-foreground">{$t('settings.payments.connect.enableHint')}</p>

	{#if enabled && list}
		<div class="mt-5 border-t pt-4">
			{#if list.accounts.length === 0}
				<p class="text-xs text-muted-foreground">{$t('settings.payments.connect.empty')}</p>
			{:else}
				<ul class="divide-y">
					{#each list.accounts as a (a.user_id)}
						{@const st = statusOf(a)}
						<li class="flex items-center justify-between gap-3 py-2.5">
							<div class="min-w-0">
								<p class="truncate text-sm font-medium">{a.user_name}</p>
								<p class="truncate text-xs text-muted-foreground">
									{a.status_error ? a.status_error : a.account_id}
								</p>
							</div>
							<div class="flex shrink-0 items-center gap-2">
								{#if st === 'ready'}
									<Badge class="bg-green-50 text-green-700 border-green-200">{$t('settings.payments.connect.statusReady')}</Badge>
								{:else if st === 'onboarding'}
									<Badge class="bg-amber-50 text-amber-700 border-amber-200" title={$t('settings.payments.connect.needsDetails')}>{$t('settings.payments.connect.statusOnboarding')}</Badge>
								{:else}
									<Badge variant="secondary">{$t('settings.payments.connect.statusError')}</Badge>
								{/if}
								<Button variant="outline" size="sm" disabled={busy} onclick={() => onboard(a)}>{$t('settings.payments.connect.onboard')}</Button>
								<Button variant="ghost" size="sm" disabled={busy} onclick={() => { unlinkTarget = a; unlinkOpen = true; }}>{$t('settings.payments.connect.unlink')}</Button>
							</div>
						</li>
					{/each}
				</ul>
			{/if}

			<div class="mt-4 flex items-center gap-2">
				{#if candidates.length > 0}
					<Select.Root
						type="single"
						value={pickUser}
						onValueChange={(v) => (pickUser = v ?? '')}
					>
						<Select.Trigger class="w-fit min-w-52">
							{candidates.find((m) => m.id === pickUser)?.name ?? $t('settings.payments.connect.pickHost')}
						</Select.Trigger>
						<Select.Content>
							{#each candidates as m (m.id)}
								<Select.Item value={m.id} label={m.name}>{m.name} · {m.email}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
					<Button disabled={busy || !pickUser} onclick={addHost}>{$t('settings.payments.connect.add')}</Button>
				{/if}
				<Button variant="ghost" disabled={busy} onclick={refresh}>{$t('settings.payments.connect.refresh')}</Button>
			</div>
		</div>
	{/if}
</div>

<ConfirmDialog
	bind:open={unlinkOpen}
	title={$t('settings.payments.connect.unlinkTitle')}
	description={$t('settings.payments.connect.unlinkDescription')}
	confirmText={$t('settings.payments.connect.unlinkConfirm')}
	destructive
	onConfirm={confirmUnlink}
/>
