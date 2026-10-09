const map = new maplibregl.Map({
    container: 'map',
    style: 'https://basemaps.cartocdn.com/gl/dark-matter-gl-style/style.json',
    center: [106.825, -6.200], // Jakarta bounding box center (see internal/config)
    zoom: 12,
    pitch: 45
});

let courierMarkers = {};
let orderMarkers = {};
let assignmentLines = {};
let pendingOfferLines = {};
let surgeZones = { type: 'FeatureCollection', features: [] };

function connect() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${protocol}//${window.location.host}/live`);

    ws.onmessage = (event) => {
        try {
            updateDashboard(JSON.parse(event.data));
        } catch (e) {
            console.error('failed to parse message', e);
        }
    };
    ws.onclose = () => setTimeout(connect, 1000);
    ws.onerror = () => ws.close();
}
connect();

function updateDashboard(data) {
    document.getElementById('val-couriers').textContent = data.couriers ? data.couriers.length : 0;
    document.getElementById('val-match-rate').textContent = data.matches || 0;

    surgeZones = data.surge_zones || surgeZones;
    const peak = surgeZones.features.reduce((m, f) => Math.max(m, f.properties.multiplier), 1);
    document.getElementById('val-surge').textContent = peak.toFixed(1) + 'x';

    let unassigned = 0;

    // Process Couriers
    const currentCouriers = new Set();
    if (data.couriers) {
        data.couriers.forEach(c => {
            currentCouriers.add(c.id);
            const lngLat = [c.lng, c.lat];
            if (!courierMarkers[c.id]) {
                const el = document.createElement('div');
                courierMarkers[c.id] = {
                    marker: new maplibregl.Marker({ element: el }).setLngLat(lngLat).addTo(map),
                    el: el
                };
            } else {
                courierMarkers[c.id].marker.setLngLat(lngLat);
            }
            // Reset to idle each tick; the offer/assignment passes below
            // promote couriers that are actually offered or assigned.
            courierMarkers[c.id].el.className = 'marker-courier courier-idle';
        });
    }

    Object.keys(courierMarkers).forEach(id => {
        if (!currentCouriers.has(id)) {
            courierMarkers[id].marker.remove();
            delete courierMarkers[id];
        }
    });

    // Process orders: markers, and (deferred) assignment line coordinates.
    const currentOrders = new Set();
    const newAssignmentLines = {};
    if (data.orders) {
        data.orders.forEach(o => {
            currentOrders.add(o.id);
            if (o.status !== 'assigned' && o.status !== 'completed') unassigned++;

            const lngLat = [o.pickup_lng, o.pickup_lat];
            if (!orderMarkers[o.id]) {
                const el = document.createElement('div');
                orderMarkers[o.id] = {
                    marker: new maplibregl.Marker({ element: el }).setLngLat(lngLat).addTo(map),
                    el: el
                };
            } else {
                orderMarkers[o.id].marker.setLngLat(lngLat);
            }

            if (o.status === 'assigned') {
                orderMarkers[o.id].el.className = 'marker-order order-assigned';
                if (o.courier_id && courierMarkers[o.courier_id]) {
                    courierMarkers[o.courier_id].el.className = 'marker-courier courier-assigned';
                    const cc = courierMarkers[o.courier_id].marker.getLngLat();
                    newAssignmentLines[o.id] = [[cc.lng, cc.lat], lngLat];
                }
            } else {
                orderMarkers[o.id].el.className = 'marker-order order-queued';
            }
        });
    }
    assignmentLines = newAssignmentLines;

    document.getElementById('val-orders').textContent = unassigned;

    Object.keys(orderMarkers).forEach(id => {
        if (!currentOrders.has(id)) {
            orderMarkers[id].marker.remove();
            delete orderMarkers[id];
        }
    });

    // Process pending offers: courier marker color, and (deferred) lines.
    const newPendingOfferLines = {};
    if (data.offers) {
        data.offers.forEach(offer => {
            if (courierMarkers[offer.courier_id]) {
                courierMarkers[offer.courier_id].el.className = 'marker-courier courier-offered';
            }
            if (!orderMarkers[offer.order_id] || !courierMarkers[offer.courier_id]) return;
            const oc = orderMarkers[offer.order_id].marker.getLngLat();
            const cc = courierMarkers[offer.courier_id].marker.getLngLat();
            newPendingOfferLines[offer.order_id + '-' + offer.courier_id] = [[cc.lng, cc.lat], [oc.lng, oc.lat]];
        });
    }
    pendingOfferLines = newPendingOfferLines;

    // One combined update instead of one setData call per line.
    updateLineSources();
}

map.on('load', () => {
    // Surge hexagons go under everything else.
    map.addSource('surge', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } });
    map.addLayer({
        id: 'surge-fill', type: 'fill', source: 'surge',
        paint: {
            'fill-color': ['interpolate', ['linear'], ['get', 'multiplier'], 1.2, '#fde047', 2, '#fb923c', 3, '#ef4444'],
            'fill-opacity': 0.35
        }
    });
    map.addLayer({
        id: 'surge-outline', type: 'line', source: 'surge',
        paint: { 'line-color': '#fb923c', 'line-width': 1, 'line-opacity': 0.7 }
    });

    map.addSource('assignments', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } });
    map.addLayer({
        id: 'assignment-lines', type: 'line', source: 'assignments',
        paint: { 'line-color': '#10b981', 'line-width': 2 }
    });

    map.addSource('pending-offers', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } });
    map.addLayer({
        id: 'pending-offer-lines', type: 'line', source: 'pending-offers',
        paint: { 'line-color': '#f43f5e', 'line-width': 2, 'line-dasharray': [4, 4] }
    });
});

function updateLineSources() {
    if (!map.isStyleLoaded()) return;

    if (map.getSource('surge')) {
        map.getSource('surge').setData(surgeZones);
    }

    if (map.getSource('assignments')) {
        const features = Object.values(assignmentLines).map(coords => (
            { type: 'Feature', geometry: { type: 'LineString', coordinates: coords } }
        ));
        map.getSource('assignments').setData({ type: 'FeatureCollection', features });
    }

    if (map.getSource('pending-offers')) {
        const features = Object.values(pendingOfferLines).map(coords => (
            { type: 'Feature', geometry: { type: 'LineString', coordinates: coords } }
        ));
        map.getSource('pending-offers').setData({ type: 'FeatureCollection', features });
    }
}
