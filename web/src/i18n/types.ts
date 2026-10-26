export type Locale = 'zh' | 'en';

export interface TranslationSchema {
  nav: {
    // Groups
    analytics: string;
    routing: string;
    modelConfig: string;
    operations: string;
    companion: string;

    // Page items
    usage: string;
    channels: string;
    models: string;
    clientAccess: string;
    modelSpecs: string;
    prices: string;
    tags: string;
    status: string;
    mcp: string;
    logs: string;
    catalog: string;
    plugins: string;
    settings: string;
    companionSettings: string;

    // Shell & Footer
    brandSubtitle: string;
    gatewayOnline: string;
    commandPalettePlaceholder: string;
    ctrlK: string;
    desktopPet: string;
    lightMode: string;
    darkMode: string;
    logout: string;
    gatewayStopped: string;
    gatewayStoppedDesc: string;
    recheckConnection: string;
  };

  common: {
    save: string;
    saving: string;
    cancel: string;
    confirm: string;
    delete: string;
    deleting: string;
    edit: string;
    copy: string;
    copied: string;
    search: string;
    loading: string;
    refresh: string;
    refreshing: string;
    filter: string;
    clearFilter: string;
    close: string;
    retry: string;
    enabled: string;
    disabled: string;
    status: string;
    actions: string;
    unknown: string;
  };

  settings: {
    title: string;
    subtitle: string;
    saveScope: string;
    saveSettings: string;
    saved: string;
    dirty: string;

    // Preferences (Language)
    preferencesTitle: string;
    preferencesDesc: string;
    interfaceLanguage: string;
    interfaceLanguageHint: string;
    langZh: string;
    langEn: string;

    // Runtime Policy
    runtimeTitle: string;
    runtimeDesc: string;
    logRetentionDays: string;
    logRetentionHint: string;
    maxAttempts: string;
    maxAttemptsHint: string;
    retriesPerChannel: string;
    retriesPerChannelHint: string;
    failureCooldown: string;
    failureCooldownHint: string;
    quotaRefresh: string;
    quotaRefreshHint: string;
    oauthAutoOpen: string;
    oauthAutoOpenHint: string;
    rulesSummary: string;
    rulesSummaryP1: string;
    rulesSummaryP2: string;
    rulesSummaryP3: string;
    rulesSummaryP4: string;

    // Protocol Conversion & Reasoning
    conversionTitle: string;
    conversionDesc: string;
    reasoningLabel: string;
    reasoningPreserve: string;
    reasoningAvailability: string;
    reasoningNote1: string;
    reasoningNote2: string;

    // Admin Login
    adminLoginTitle: string;
    adminLoginDesc: string;
    gatewayKey: string;
    gatewayKeyHint: string;
    hideKey: string;
    showKey: string;
    copyKey: string;

    // Request Defaults
    defaultsTitle: string;
    defaultsDesc: string;
    defaultsToggle: string;
    defaultsEnabled: string;
    defaultsDefaultOff: string;
    defaultsNote: string;
    defaultsEdit: string;
    defaultsEditDesc: string;

    // Backup & Restore
    backupTitle: string;
    backupDesc: string;
    exportCardTitle: string;
    exportCardDesc: string;
    exportPassword: string;
    exportPasswordHint: string;
    exportButton: string;
    restoreCardTitle: string;
    restoreCardDesc: string;
    restoreFile: string;
    restoreFileHint: string;
    restoreFileSelected: string;
    restorePassword: string;
    restorePasswordHint: string;
    previewButton: string;
    parsing: string;
    restoreButton: string;
    previewSummaryTitle: string;
    previewChannels: string;
    previewModels: string;
    previewKeys: string;
    previewPrices: string;
    previewTags: string;
    previewExportTime: string;
    rollbackCardTitle: string;
    rollbackCardDesc: string;
    latestSnapshot: string;
    rollbackButton: string;
    rollingBack: string;

    // Service Control
    serviceTitle: string;
    serviceDesc: string;
    portLabel: string;
    portHintActual: string;
    portHintWaiting: string;
    portHintRestartRequired: string;
    savePort: string;
    savingPort: string;
    portNoteSaved: string;
    portNoteUnsaved: string;
    portNoteDirty: string;
    portExplanationSummary: string;
    portExplanationP1: string;
    portExplanationP2: string;
    restartService: string;
    shutdownService: string;

    // Modals
    restartModalTitle: string;
    restartModalDesc: string;
    shutdownModalTitle: string;
    shutdownModalDesc: string;
    restoreModalTitle: string;
    restoreModalDesc: string;
    rollbackModalTitle: string;
    rollbackModalDesc: string;
    confirmRestart: string;
    confirmShutdown: string;
    confirmRestore: string;
    confirmRollback: string;
    processing: string;

    // Legal
    thirdPartyNotices: string;
  };

  login: {
    title: string;
    subtitle: string;
    intro: string;
    keyLabel: string;
    keyPlaceholder: string;
    rememberMe: string;
    submit: string;
    verifying: string;
    footnote: string;
  };

  commandPalette: {
    placeholder: string;
    searchHint: string;
    navigationCategory: string;
    actionsCategory: string;
    toggleThemeTitle: string;
    toggleThemeDarkDesc: string;
    toggleThemeLightDesc: string;
    copyAddressTitle: string;
    copyAddressDesc: string;
    logoutTitle: string;
    logoutDesc: string;
    noResults: string;
  };
}
