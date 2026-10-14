import json,time,sys,os,shutil
from playwright.sync_api import sync_playwright
fixture=sys.argv[1] if len(sys.argv)>1 else '/tmp/reserve-ui-render.html'
html=open(fixture).read()
status=json.load(open(fixture+'.json'))
now=time.time()
history={'a':[{'at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime(now-3600)),'five_hour':55,'long_window':60,'event':'reset'},{'at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime(now)),'five_hour':50,'long_window':50,'event':'protection'}]}
with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=os.environ.get('CHROMIUM_PATH') or shutil.which('chromium'),headless=True,args=['--no-sandbox'])
 page=browser.new_page();errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
 def route(r):
  url=r.request.url
  if '/v0/management' not in url: body=html;kind='text/html'
  elif '/history' in url: body=json.dumps(history);kind='application/json'
  elif '/status' in url or '/settings' in url: body=json.dumps(status);kind='application/json'
  elif 'v0/management' in url: body='{}';kind='application/json'
  else:body=html;kind='text/html'
  r.fulfill(status=200,content_type=kind,body=body)
 page.route('**/*',route)
 page.goto('http://reserve.test/plugins/codex-quota-reserve/status')
 page.fill('#managementKey','integration-fixture-key')
 if page.locator('#loadData').is_visible(): page.click('#loadData')
 page.wait_for_selector('article.card')
 page.click('#historyLoad')
 page.wait_for_selector('#historyChart polyline')
 assert page.locator('#historyChart circle').count()==2
 page.locator('button.openEdit').first.click()
 assert page.locator('#editReserve5H').is_visible()
 assert page.locator('#editReserveLong').input_value()==''
 page.locator('#editDialog').evaluate('(dialog)=>dialog.close()')
 page.locator('#settingsPanel summary').click()
 assert page.locator('#reserve5H').input_value()=='10'
 assert page.locator('#reserveMargin').input_value()=='2'
 assert page.locator('#reserveStrict').is_checked()
 assert not errors,errors
 print('Browser passed: settings initialization, account editor, history curve, stop line and event markers; no JavaScript errors')
 browser.close()
