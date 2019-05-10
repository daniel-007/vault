/**
 * @module HttpRequestsTable
 * HttpRequestsTable components render a table with the total number of HTTP Requests to a Vault server per month.
 *
 * @example
 * ```js
 * const COUNTERS = [
 *    {
 *       "start_time": "2019-05-01T00:00:00Z",
 *       "total": 50
 *     }
 * ]
 *
 * <HttpRequestsTable @counters={{COUNTERS}} />
 * ```
 *
 * @param counters {Array} - A list of objects containing the total number of HTTP Requests for each month. `counters` should be the response from the `/internal/counters/requests` endpoint.
 */

import Component from '@ember/component';
import { computed } from '@ember/object';

export default Component.extend({
  counters: null,
  showChangeColumn: computed('counters', function() {
    const { counters } = this;
    return counters && counters.length > 1;
  }),
});
