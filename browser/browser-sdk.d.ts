/** 当前应用的可信浏览器运行时上下文。 */
export interface MetisBrowserContext {
  appId: string;
  appType: "RUNTIME_APPLICATION_TYPE_WEB";
  tenantId: string;
  userId: string;
  role: string;
  actorType: "RUNTIME_ACTOR_TYPE_USER";
}

/** source 应用声明的 Web 依赖。 */
export interface MetisWebDependency {
  appId: string;
  alias: string;
  required: boolean;
  appType: "RUNTIME_APPLICATION_TYPE_WEB";
  available: boolean;
  webBasePath: string;
}

/** 平台注入到已安装 Web 应用中的浏览器能力。 */
export interface MetisBrowserSDK {
  appURL(path?: string): string;
  getContext(): Promise<MetisBrowserContext>;
  listDependencies(): Promise<MetisWebDependency[]>;
  dependency(aliasOrAppId: string): Promise<MetisWebDependency>;
  dependencyURL(aliasOrAppId: string, path?: string): Promise<string>;
  openAppPage(options: { app: string; path?: string; query?: Record<string, string> }): Promise<void>;
}

declare global {
  interface Window {
    Metis: MetisBrowserSDK;
  }
}

export {};
