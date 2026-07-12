export function Badge({
  children,
  className = "",
  title,
}: {
  children: React.ReactNode;
  className?: string;
  title?: string;
}) {
  return (
    <span className={`badge ${className}`} title={title}>
      {children}
    </span>
  );
}
