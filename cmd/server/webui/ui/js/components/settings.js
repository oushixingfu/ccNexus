import { api } from '../api.js';
import { state } from '../state.js';
import { notifications } from '../utils/notifications.js';
import { t } from '../utils/i18n.js';

const defaultFailover = {
    recoveredEndpointPolicy: 'deprioritize',
    cooldowns: {
        quotaExhaustedSec: 3600,
        rateLimitedSec: 120,
        upstreamErrorSec: 60,
        networkErrorSec: 30,
        tokenUnavailableSec: 600,
        configErrorSec: 1800
    }
};

const defaultUnifiedModel = {
    enabled: false,
    name: 'gpt-5.5',
    aliases: [],
    advertiseOnlyUnifiedModel: true,
    endpointScope: 'all_enabled',
    hotStandby: true,
    preserveExplicitEndpointOverride: true
};

class Settings {
    constructor() {
        this.container = document.getElementById('view-container');
        window.addEventListener('languageChanged', () => {
            if (state.get('currentView') === 'settings') {
                this.render();
            }
        });
    }

    async render() {
        this.container.innerHTML = `
            <div class="settings">
                <h1>${t('settings.title')}</h1>
                <form id="settings-form">
                    <div class="card mt-3">
                        <div class="card-header">
                            <h3 class="card-title">${t('settings.unifiedModelTitle')}</h3>
                        </div>
                        <div class="card-body">
                            <label class="checkbox-row">
                                <input type="checkbox" class="form-checkbox" name="unifiedModelEnabled">
                                <span>${t('settings.unifiedModel.enabled')}</span>
                            </label>
                            <div class="grid grid-cols-2">
                                <div class="form-group">
                                    <label class="form-label">${t('settings.unifiedModel.name')}</label>
                                    <input class="form-input" type="text" name="unifiedModelName" autocomplete="off">
                                </div>
                                <div class="form-group">
                                    <label class="form-label">${t('settings.unifiedModel.aliases')}</label>
                                    <input class="form-input" type="text" name="unifiedModelAliases" autocomplete="off" placeholder="gpt-auto">
                                </div>
                            </div>
                            <label class="checkbox-row">
                                <input type="checkbox" class="form-checkbox" name="unifiedModelAdvertiseOnly">
                                <span>${t('settings.unifiedModel.advertiseOnly')}</span>
                            </label>
                            <label class="checkbox-row">
                                <input type="checkbox" class="form-checkbox" name="unifiedModelHotStandby">
                                <span>${t('settings.unifiedModel.hotStandby')}</span>
                            </label>
                            <label class="checkbox-row">
                                <input type="checkbox" class="form-checkbox" name="unifiedModelPreserveOverride">
                                <span>${t('settings.unifiedModel.preserveOverride')}</span>
                            </label>
                        </div>
                    </div>

                    <div class="card mt-3">
                        <div class="card-header">
                            <h3 class="card-title">${t('settings.failoverTitle')}</h3>
                        </div>
                        <div class="card-body">
                            <div class="form-group">
                                <label class="form-label">${t('settings.routingStrategy')}</label>
                                <select class="form-select" name="routingStrategy">
                                    <option value="auto">${t('settings.routingStrategies.auto')}</option>
                                    <option value="claude">${t('settings.routingStrategies.claude')}</option>
                                    <option value="codex">${t('settings.routingStrategies.codex')}</option>
                                </select>
                            </div>
                            <div class="form-group">
                                <label class="form-label">${t('settings.recoveredEndpointPolicy')}</label>
                                <select class="form-select" name="recoveredEndpointPolicy">
                                    <option value="deprioritize">${t('settings.policies.deprioritize')}</option>
                                    <option value="auto_return">${t('settings.policies.autoReturn')}</option>
                                </select>
                            </div>
                            <div class="grid grid-cols-2">
                                ${this.renderCooldownInput('quotaExhaustedSec', t('settings.cooldowns.quotaExhausted'))}
                                ${this.renderCooldownInput('rateLimitedSec', t('settings.cooldowns.rateLimited'))}
                                ${this.renderCooldownInput('upstreamErrorSec', t('settings.cooldowns.upstreamError'))}
                                ${this.renderCooldownInput('networkErrorSec', t('settings.cooldowns.networkError'))}
                                ${this.renderCooldownInput('tokenUnavailableSec', t('settings.cooldowns.tokenUnavailable'))}
                                ${this.renderCooldownInput('configErrorSec', t('settings.cooldowns.configError'))}
                            </div>
                        </div>
                    </div>
                    <button type="submit" class="btn btn-primary mt-3">${t('common.save')}</button>
                </form>
            </div>
        `;

        document.getElementById('settings-form').addEventListener('submit', (event) => this.save(event));
        await this.load();
    }

    renderCooldownInput(name, label) {
        return `
            <div class="form-group">
                <label class="form-label">${label}</label>
                <input class="form-input" type="number" min="0" name="${name}">
            </div>
        `;
    }

    async load() {
        try {
            const config = await api.getConfig();
            const failover = this.normalizeFailover(config.failover);
            const unifiedModel = this.normalizeUnifiedModel(config.unifiedModel);
            const form = document.getElementById('settings-form');
            form.elements.unifiedModelEnabled.checked = unifiedModel.enabled;
            form.elements.unifiedModelName.value = unifiedModel.name;
            form.elements.unifiedModelAliases.value = unifiedModel.aliases.join(', ');
            form.elements.unifiedModelAdvertiseOnly.checked = unifiedModel.advertiseOnlyUnifiedModel;
            form.elements.unifiedModelHotStandby.checked = unifiedModel.hotStandby;
            form.elements.unifiedModelPreserveOverride.checked = unifiedModel.preserveExplicitEndpointOverride;
            form.elements.routingStrategy.value = this.normalizeRoutingStrategy(config.routingStrategy);
            form.elements.recoveredEndpointPolicy.value = failover.recoveredEndpointPolicy;
            Object.entries(failover.cooldowns).forEach(([key, value]) => {
                if (form.elements[key]) {
                    form.elements[key].value = value;
                }
            });
        } catch (error) {
            notifications.error(`${t('settings.failedToLoad')}: ${error.message}`);
        }
    }

    async save(event) {
        event.preventDefault();
        const form = event.currentTarget;
        const readSeconds = (name) => {
            const value = Number.parseInt(form.elements[name]?.value || '0', 10);
            return Number.isFinite(value) && value > 0 ? value : 0;
        };

        try {
            const unifiedModelName = form.elements.unifiedModelName.value.trim() || defaultUnifiedModel.name;
            await api.updateConfig({
                unifiedModel: {
                    enabled: form.elements.unifiedModelEnabled.checked,
                    name: unifiedModelName,
                    aliases: this.parseAliases(form.elements.unifiedModelAliases.value, unifiedModelName),
                    advertiseOnlyUnifiedModel: form.elements.unifiedModelAdvertiseOnly.checked,
                    endpointScope: defaultUnifiedModel.endpointScope,
                    hotStandby: form.elements.unifiedModelHotStandby.checked,
                    preserveExplicitEndpointOverride: form.elements.unifiedModelPreserveOverride.checked
                },
                routingStrategy: this.normalizeRoutingStrategy(form.elements.routingStrategy.value),
                failover: {
                    recoveredEndpointPolicy: form.elements.recoveredEndpointPolicy.value,
                    cooldowns: {
                        quotaExhaustedSec: readSeconds('quotaExhaustedSec'),
                        rateLimitedSec: readSeconds('rateLimitedSec'),
                        upstreamErrorSec: readSeconds('upstreamErrorSec'),
                        networkErrorSec: readSeconds('networkErrorSec'),
                        tokenUnavailableSec: readSeconds('tokenUnavailableSec'),
                        configErrorSec: readSeconds('configErrorSec')
                    }
                }
            });
            notifications.success(t('settings.saved'));
            await this.load();
        } catch (error) {
            notifications.error(`${t('settings.failedToSave')}: ${error.message}`);
        }
    }

    normalizeUnifiedModel(unifiedModel) {
        const aliases = Array.isArray(unifiedModel?.aliases) ? unifiedModel.aliases : defaultUnifiedModel.aliases;
        return {
            enabled: Boolean(unifiedModel?.enabled),
            name: typeof unifiedModel?.name === 'string' && unifiedModel.name.trim() ? unifiedModel.name.trim() : defaultUnifiedModel.name,
            aliases: aliases.filter((alias) => typeof alias === 'string' && alias.trim()).map((alias) => alias.trim()),
            advertiseOnlyUnifiedModel: unifiedModel?.advertiseOnlyUnifiedModel !== false,
            endpointScope: unifiedModel?.endpointScope || defaultUnifiedModel.endpointScope,
            hotStandby: unifiedModel?.hotStandby !== false,
            preserveExplicitEndpointOverride: unifiedModel?.preserveExplicitEndpointOverride !== false
        };
    }

    parseAliases(value, modelName) {
        const seen = new Set([modelName.toLowerCase()]);
        return String(value || '')
            .split(',')
            .map((alias) => alias.trim())
            .filter((alias) => {
                const key = alias.toLowerCase();
                if (!alias || seen.has(key)) {
                    return false;
                }
                seen.add(key);
                return true;
            });
    }

    normalizeFailover(failover) {
        const cooldowns = failover?.cooldowns || {};
        return {
            recoveredEndpointPolicy: failover?.recoveredEndpointPolicy || defaultFailover.recoveredEndpointPolicy,
            cooldowns: {
                quotaExhaustedSec: Number.isFinite(Number(cooldowns.quotaExhaustedSec)) ? Number(cooldowns.quotaExhaustedSec) : defaultFailover.cooldowns.quotaExhaustedSec,
                rateLimitedSec: Number.isFinite(Number(cooldowns.rateLimitedSec)) ? Number(cooldowns.rateLimitedSec) : defaultFailover.cooldowns.rateLimitedSec,
                upstreamErrorSec: Number.isFinite(Number(cooldowns.upstreamErrorSec)) ? Number(cooldowns.upstreamErrorSec) : defaultFailover.cooldowns.upstreamErrorSec,
                networkErrorSec: Number.isFinite(Number(cooldowns.networkErrorSec)) ? Number(cooldowns.networkErrorSec) : defaultFailover.cooldowns.networkErrorSec,
                tokenUnavailableSec: Number.isFinite(Number(cooldowns.tokenUnavailableSec)) ? Number(cooldowns.tokenUnavailableSec) : defaultFailover.cooldowns.tokenUnavailableSec,
                configErrorSec: Number.isFinite(Number(cooldowns.configErrorSec)) ? Number(cooldowns.configErrorSec) : defaultFailover.cooldowns.configErrorSec
            }
        };
    }

    normalizeRoutingStrategy(strategy) {
        if (strategy === 'claude' || strategy === 'codex') {
            return strategy;
        }
        return 'auto';
    }
}

export const settings = new Settings();
