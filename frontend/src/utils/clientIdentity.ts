export const SOURCE_FEED_GROUP = '@source-feed'

export const isSystemSubscriptionClient = (client: { group?: string }): boolean =>
  client.group === SOURCE_FEED_GROUP

export const clientGroupLabel = (group: string, t: (key: string) => string): string =>
  group === SOURCE_FEED_GROUP ? t('ui.systemSubscriptionGroup') : group || t('none')

export const clientDisplayName = (
  client: { name?: string; desc?: string; group?: string },
  t: (key: string) => string,
): string => isSystemSubscriptionClient(client)
  ? client.desc || t('ui.systemSubscriptionAccount')
  : client.name || ''
