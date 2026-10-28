import { useMemo, useState, type ReactNode } from 'react';
import { ArrowDown, ArrowUp } from 'lucide-react';

export interface Column<T> {
  key: string;
  header: ReactNode;
  render: (row: T) => ReactNode;
  /** numeric columns are right-aligned, tabular figures */
  numeric?: boolean;
  sortValue?: (row: T) => number | string;
  width?: number | string;
}

interface Props<T> {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  onRowClick?: (row: T) => void;
  defaultSort?: { key: string; dir: 'asc' | 'desc' };
  empty?: ReactNode;
  /** Nested rows (e.g. the functions of an Azure Function App): rendered indented right below their parent, sorted the same way. */
  childRows?: (row: T) => T[] | undefined;
}

export function Table<T>({ columns, rows, rowKey, onRowClick, defaultSort, empty, childRows }: Props<T>) {
  const [sort, setSort] = useState(defaultSort);
  const sortRows = useMemo(() => {
    const col = sort && columns.find((c) => c.key === sort.key);
    if (!sort || !col?.sortValue) return (xs: T[]) => xs;
    const get = col.sortValue;
    return (xs: T[]) => [...xs].sort((a, b) => {
      const va = get(a), vb = get(b);
      const r = va < vb ? -1 : va > vb ? 1 : 0;
      return sort.dir === 'asc' ? r : -r;
    });
  }, [sort, columns]);
  const sorted = useMemo(() => sortRows(rows).flatMap((r) => [{ r, child: false }, ...sortRows(childRows?.(r) ?? []).map((c) => ({ r: c, child: true }))]),
    [rows, sortRows, childRows]);

  if (rows.length === 0 && empty) return <>{empty}</>;

  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            {columns.map((c) => {
              const active = sort?.key === c.key;
              const sortable = !!c.sortValue;
              return (
                <th
                  key={c.key}
                  className={[c.numeric && 'is-num', sortable && 'is-sortable'].filter(Boolean).join(' ')}
                  style={{ width: c.width }}
                  aria-sort={active ? (sort!.dir === 'asc' ? 'ascending' : 'descending') : undefined}
                  onClick={sortable ? () => setSort({ key: c.key, dir: active && sort!.dir === 'desc' ? 'asc' : 'desc' }) : undefined}
                >
                  <span className="row" style={{ gap: 2, justifyContent: c.numeric ? 'flex-end' : 'flex-start', display: 'inline-flex' }}>
                    {c.header}
                    {active && (sort!.dir === 'asc' ? <ArrowUp size={12} /> : <ArrowDown size={12} />)}
                  </span>
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {sorted.map(({ r, child }) => (
            <tr key={rowKey(r)} className={[onRowClick && 'is-clickable', child && 'is-child'].filter(Boolean).join(' ') || undefined}
              onClick={onRowClick ? () => onRowClick(r) : undefined}>
              {columns.map((c) => (
                <td key={c.key} className={c.numeric ? 'is-num' : undefined}>{c.render(r)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
