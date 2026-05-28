import { t } from './i18n.js';

export function formatAvailabilityReason(reason) {
    const normalized = (reason || '').trim();
    const labels = {
        quota_exhausted: t('endpoints.reasonQuotaExhausted'),
        rate_limited: t('endpoints.reasonRateLimited'),
        upstream_5xx: t('endpoints.reasonUpstreamError'),
        retryable_status: t('endpoints.reasonUpstreamError'),
        upstream_stream_error: t('endpoints.reasonStreamError'),
        streaming_failed: t('endpoints.reasonStreamError'),
        aggregate_streaming_failed: t('endpoints.reasonStreamError'),
        send_request_failed: t('endpoints.reasonNetworkError'),
        transient_network_error: t('endpoints.reasonNetworkError'),
        transport_protocol_error: t('endpoints.reasonNetworkError'),
        endpoint_auth_failed: t('endpoints.reasonAuthFailed'),
        credential_select_failed: t('endpoints.reasonTokenUnavailable'),
        no_usable_token: t('endpoints.reasonTokenUnavailable'),
        credential_refresh_failed: t('endpoints.reasonTokenUnavailable'),
        empty_api_key: t('endpoints.reasonConfigError'),
        prepare_transformer_failed: t('endpoints.reasonConfigError'),
        build_request_failed: t('endpoints.reasonConfigError')
    };
    return labels[normalized] || normalized || t('endpoints.unavailable');
}

export function endpointAvailabilityTitle(endpoint) {
    const reason = formatAvailabilityReason(endpoint?.availabilityReason);
    const code = endpoint?.availabilityStatusCode ? `HTTP ${endpoint.availabilityStatusCode}` : '';
    const lastFailureAt = endpoint?.runtimeStatus?.lastFailureAt
        ? new Date(endpoint.runtimeStatus.lastFailureAt).toLocaleString()
        : '';
    const parts = [reason, code, lastFailureAt].filter(Boolean);
    return parts.length > 0 ? parts.join(' · ') : t('endpoints.unavailable');
}

export function endpointAvailability(endpoint) {
    if (!endpoint || endpoint.enabled === false) {
        return {
            available: false,
            availability: 'disabled',
            badgeClass: 'badge-danger',
            indicatorClass: 'offline',
            label: t('common.disabled'),
            title: t('common.disabled'),
            reason: ''
        };
    }

    const availability = endpoint.availability || (endpoint.available === false ? 'unknown' : 'available');
    if (availability === 'unavailable') {
        const reason = formatAvailabilityReason(endpoint.availabilityReason);
        return {
            available: false,
            availability: 'unavailable',
            badgeClass: 'badge-danger',
            indicatorClass: 'offline',
            label: t('endpoints.unavailable'),
            title: endpointAvailabilityTitle(endpoint),
            reason
        };
    }

    if (availability === 'unknown' || endpoint.available !== true) {
        return {
            available: false,
            availability: 'unknown',
            badgeClass: 'badge-warning',
            indicatorClass: 'unknown',
            label: t('endpoints.notTested'),
            title: t('endpoints.notTested'),
            reason: ''
        };
    }

    return {
        available: true,
        availability: 'available',
        badgeClass: 'badge-success',
        indicatorClass: 'online',
        label: t('endpoints.available'),
        title: t('endpoints.available'),
        reason: ''
    };
}
