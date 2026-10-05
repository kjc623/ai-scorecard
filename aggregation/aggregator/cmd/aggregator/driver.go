// Without the sac_sql_driver build tag this binary carries no PostgreSQL driver. The flag is left
// empty and -store sql refuses with an actionable message rather than starting and doing nothing.
package main

var defaultDriverName = ""
