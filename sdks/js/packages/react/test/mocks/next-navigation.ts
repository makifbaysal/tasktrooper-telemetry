export const nav = { pathname: '/' as string | null, params: {} as Record<string, string | string[]> | null };
export const usePathname = () => nav.pathname;
export const useParams = () => nav.params;
