export namespace model {
	
	export class Article {
	    id: string;
	    title: string;
	    content?: string;
	    url: string;
	    sourceId: string;
	    sourceName: string;
	    sourceKind: string;
	    channel: string;
	    date: string;
	    dateText: string;
	    dateInferred: boolean;
	    fetchedAt: string;
	
	    static createFrom(source: any = {}) {
	        return new Article(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.content = source["content"];
	        this.url = source["url"];
	        this.sourceId = source["sourceId"];
	        this.sourceName = source["sourceName"];
	        this.sourceKind = source["sourceKind"];
	        this.channel = source["channel"];
	        this.date = source["date"];
	        this.dateText = source["dateText"];
	        this.dateInferred = source["dateInferred"];
	        this.fetchedAt = source["fetchedAt"];
	    }
	}
	export class Channel {
	    id: string;
	    name: string;
	    url: string;
	
	    static createFrom(source: any = {}) {
	        return new Channel(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.url = source["url"];
	    }
	}
	export class SyncStatus {
	    running: boolean;
	    phase: string;
	    current: string;
	    completed: number;
	    total: number;
	    startedAt: string;
	    finishedAt: string;
	    errors: string[];
	
	    static createFrom(source: any = {}) {
	        return new SyncStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.phase = source["phase"];
	        this.current = source["current"];
	        this.completed = source["completed"];
	        this.total = source["total"];
	        this.startedAt = source["startedAt"];
	        this.finishedAt = source["finishedAt"];
	        this.errors = source["errors"];
	    }
	}
	export class Source {
	    id: string;
	    name: string;
	    url: string;
	    kind: string;
	    group: string;
	    channels: Channel[];
	    status: string;
	    lastCrawled: string;
	    error: string;
	    articleCount: number;
	
	    static createFrom(source: any = {}) {
	        return new Source(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.url = source["url"];
	        this.kind = source["kind"];
	        this.group = source["group"];
	        this.channels = this.convertValues(source["channels"], Channel);
	        this.status = source["status"];
	        this.lastCrawled = source["lastCrawled"];
	        this.error = source["error"];
	        this.articleCount = source["articleCount"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Snapshot {
	    sources: Source[];
	    articles: Article[];
	    sync: SyncStatus;
	    lastUpdated: string;
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sources = this.convertValues(source["sources"], Source);
	        this.articles = this.convertValues(source["articles"], Article);
	        this.sync = this.convertValues(source["sync"], SyncStatus);
	        this.lastUpdated = source["lastUpdated"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class SyncOptions {
	    sourceId: string;
	    pagesPerChannel: number;
	    maxChannelsPerSource: number;
	
	    static createFrom(source: any = {}) {
	        return new SyncOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sourceId = source["sourceId"];
	        this.pagesPerChannel = source["pagesPerChannel"];
	        this.maxChannelsPerSource = source["maxChannelsPerSource"];
	    }
	}

}

